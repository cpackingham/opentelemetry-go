// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package opentracing

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	ot "github.com/opentracing/opentracing-go"
	otext "github.com/opentracing/opentracing-go/ext"
	otlog "github.com/opentracing/opentracing-go/log"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	otelglobal "go.opentelemetry.io/otel/global"
	"go.opentelemetry.io/otel/internal/baggage"
	"go.opentelemetry.io/otel/internal/trace/noop"
	otelparent "go.opentelemetry.io/otel/internal/trace/parent"
	"go.opentelemetry.io/otel/label"

	"go.opentelemetry.io/otel/bridge/opentracing/migration"
)

type bridgeSpanReference struct {
	baggageItems      baggage.Map
	otelSpanReference otel.SpanReference
}

var _ ot.SpanContext = &bridgeSpanReference{}

func newBridgeSpanReference(otelSpanReference otel.SpanReference, parentOtSpanContext ot.SpanContext) *bridgeSpanReference {
	bRef := &bridgeSpanReference{
		baggageItems:      baggage.NewEmptyMap(),
		otelSpanReference: otelSpanReference,
	}
	if parentOtSpanContext != nil {
		parentOtSpanContext.ForeachBaggageItem(func(key, value string) bool {
			bRef.setBaggageItem(key, value)
			return true
		})
	}
	return bRef
}

func (c *bridgeSpanReference) ForeachBaggageItem(handler func(k, v string) bool) {
	c.baggageItems.Foreach(func(kv label.KeyValue) bool {
		return handler(string(kv.Key), kv.Value.Emit())
	})
}

func (c *bridgeSpanReference) setBaggageItem(restrictedKey, value string) {
	crk := http.CanonicalHeaderKey(restrictedKey)
	c.baggageItems = c.baggageItems.Apply(baggage.MapUpdate{SingleKV: label.String(crk, value)})
}

func (c *bridgeSpanReference) baggageItem(restrictedKey string) string {
	crk := http.CanonicalHeaderKey(restrictedKey)
	val, _ := c.baggageItems.Value(label.Key(crk))
	return val.Emit()
}

type bridgeSpan struct {
	otelSpan          otel.Span
	ctx               *bridgeSpanReference
	tracer            *BridgeTracer
	skipDeferHook     bool
	extraBaggageItems map[string]string
}

var _ ot.Span = &bridgeSpan{}

func newBridgeSpan(otelSpan otel.Span, bridgeSC *bridgeSpanReference, tracer *BridgeTracer) *bridgeSpan {
	return &bridgeSpan{
		otelSpan:          otelSpan,
		ctx:               bridgeSC,
		tracer:            tracer,
		skipDeferHook:     false,
		extraBaggageItems: nil,
	}
}

func (s *bridgeSpan) Finish() {
	s.otelSpan.End()
}

func (s *bridgeSpan) FinishWithOptions(opts ot.FinishOptions) {
	var otelOpts []otel.SpanOption

	if !opts.FinishTime.IsZero() {
		otelOpts = append(otelOpts, otel.WithTimestamp(opts.FinishTime))
	}
	for _, record := range opts.LogRecords {
		s.logRecord(record)
	}
	for _, data := range opts.BulkLogData {
		s.logRecord(data.ToLogRecord())
	}
	s.otelSpan.End(otelOpts...)
}

func (s *bridgeSpan) logRecord(record ot.LogRecord) {
	s.otelSpan.AddEvent(
		"",
		otel.WithTimestamp(record.Timestamp),
		otel.WithAttributes(otLogFieldsToOTelLabels(record.Fields)...),
	)
}

func (s *bridgeSpan) Context() ot.SpanContext {
	return s.ctx
}

func (s *bridgeSpan) SetOperationName(operationName string) ot.Span {
	s.otelSpan.SetName(operationName)
	return s
}

func (s *bridgeSpan) SetTag(key string, value interface{}) ot.Span {
	switch key {
	case string(otext.SpanKind):
		// TODO: Should we ignore it?
	case string(otext.Error):
		if b, ok := value.(bool); ok && b {
			s.otelSpan.SetStatus(codes.Error, "")
		}
	default:
		s.otelSpan.SetAttributes(otTagToOTelLabel(key, value))
	}
	return s
}

func (s *bridgeSpan) LogFields(fields ...otlog.Field) {
	s.otelSpan.AddEvent(
		"",
		otel.WithAttributes(otLogFieldsToOTelLabels(fields)...),
	)
}

type bridgeFieldEncoder struct {
	pairs []label.KeyValue
}

var _ otlog.Encoder = &bridgeFieldEncoder{}

func (e *bridgeFieldEncoder) EmitString(key, value string) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitBool(key string, value bool) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitInt(key string, value int) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitInt32(key string, value int32) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitInt64(key string, value int64) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitUint32(key string, value uint32) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitUint64(key string, value uint64) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitFloat32(key string, value float32) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitFloat64(key string, value float64) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitObject(key string, value interface{}) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitLazyLogger(value otlog.LazyLogger) {
	value(e)
}

func (e *bridgeFieldEncoder) emitCommon(key string, value interface{}) {
	e.pairs = append(e.pairs, otTagToOTelLabel(key, value))
}

func otLogFieldsToOTelLabels(fields []otlog.Field) []label.KeyValue {
	encoder := &bridgeFieldEncoder{}
	for _, field := range fields {
		field.Marshal(encoder)
	}
	return encoder.pairs
}

func (s *bridgeSpan) LogKV(alternatingKeyValues ...interface{}) {
	fields, err := otlog.InterleavedKVToFields(alternatingKeyValues...)
	if err != nil {
		return
	}
	s.LogFields(fields...)
}

func (s *bridgeSpan) SetBaggageItem(restrictedKey, value string) ot.Span {
	s.updateOTelContext(restrictedKey, value)
	s.setBaggageItemOnly(restrictedKey, value)
	return s
}

func (s *bridgeSpan) setBaggageItemOnly(restrictedKey, value string) {
	s.ctx.setBaggageItem(restrictedKey, value)
}

func (s *bridgeSpan) updateOTelContext(restrictedKey, value string) {
	if s.extraBaggageItems == nil {
		s.extraBaggageItems = make(map[string]string)
	}
	s.extraBaggageItems[restrictedKey] = value
}

func (s *bridgeSpan) BaggageItem(restrictedKey string) string {
	return s.ctx.baggageItem(restrictedKey)
}

func (s *bridgeSpan) Tracer() ot.Tracer {
	return s.tracer
}

func (s *bridgeSpan) LogEvent(event string) {
	s.LogEventWithPayload(event, nil)
}

func (s *bridgeSpan) LogEventWithPayload(event string, payload interface{}) {
	data := ot.LogData{
		Event:   event,
		Payload: payload,
	}
	s.Log(data)
}

func (s *bridgeSpan) Log(data ot.LogData) {
	record := data.ToLogRecord()
	s.LogFields(record.Fields...)
}

type bridgeSetTracer struct {
	isSet      bool
	otelTracer otel.Tracer

	warningHandler BridgeWarningHandler
	warnOnce       sync.Once
}

func (s *bridgeSetTracer) tracer() otel.Tracer {
	if !s.isSet {
		s.warnOnce.Do(func() {
			s.warningHandler("The OpenTelemetry tracer is not set, default no-op tracer is used! Call SetOpenTelemetryTracer to set it up.\n")
		})
	}
	return s.otelTracer
}

// BridgeWarningHandler is a type of handler that receives warnings
// from the BridgeTracer.
type BridgeWarningHandler func(msg string)

// BridgeTracer is an implementation of the OpenTracing tracer, which
// translates the calls to the OpenTracing API into OpenTelemetry
// counterparts and calls the underlying OpenTelemetry tracer.
type BridgeTracer struct {
	setTracer bridgeSetTracer

	warningHandler BridgeWarningHandler
	warnOnce       sync.Once

	propagator otel.TextMapPropagator
}

var _ ot.Tracer = &BridgeTracer{}
var _ ot.TracerContextWithSpanExtension = &BridgeTracer{}

// NewBridgeTracer creates a new BridgeTracer. The new tracer forwards
// the calls to the OpenTelemetry Noop tracer, so it should be
// overridden with the SetOpenTelemetryTracer function. The warnings
// handler does nothing by default, so to override it use the
// SetWarningHandler function.
func NewBridgeTracer() *BridgeTracer {
	return &BridgeTracer{
		setTracer: bridgeSetTracer{
			otelTracer: noop.Tracer,
		},
		warningHandler: func(msg string) {},
		propagator:     nil,
	}
}

// SetWarningHandler overrides the warning handler.
func (t *BridgeTracer) SetWarningHandler(handler BridgeWarningHandler) {
	t.setTracer.warningHandler = handler
	t.warningHandler = handler
}

// SetWarningHandler overrides the underlying OpenTelemetry
// tracer. The passed tracer should know how to operate in the
// environment that uses OpenTracing API.
func (t *BridgeTracer) SetOpenTelemetryTracer(tracer otel.Tracer) {
	t.setTracer.otelTracer = tracer
	t.setTracer.isSet = true
}

func (t *BridgeTracer) SetTextMapPropagator(propagator otel.TextMapPropagator) {
	t.propagator = propagator
}

func (t *BridgeTracer) NewHookedContext(ctx context.Context) context.Context {
	ctx = baggage.ContextWithSetHook(ctx, t.baggageSetHook)
	ctx = baggage.ContextWithGetHook(ctx, t.baggageGetHook)
	return ctx
}

func (t *BridgeTracer) baggageSetHook(ctx context.Context) context.Context {
	span := ot.SpanFromContext(ctx)
	if span == nil {
		t.warningHandler("No active OpenTracing span, can not propagate the baggage items from OpenTelemetry context\n")
		return ctx
	}
	bSpan, ok := span.(*bridgeSpan)
	if !ok {
		t.warningHandler("Encountered a foreign OpenTracing span, will not propagate the baggage items from OpenTelemetry context\n")
		return ctx
	}
	// we clear the context only to avoid calling a get hook
	// during MapFromContext, but otherwise we don't change the
	// context, so we don't care about the old hooks.
	clearCtx, _, _ := baggage.ContextWithNoHooks(ctx)
	m := baggage.MapFromContext(clearCtx)
	m.Foreach(func(kv label.KeyValue) bool {
		bSpan.setBaggageItemOnly(string(kv.Key), kv.Value.Emit())
		return true
	})
	return ctx
}

func (t *BridgeTracer) baggageGetHook(ctx context.Context, m baggage.Map) baggage.Map {
	span := ot.SpanFromContext(ctx)
	if span == nil {
		t.warningHandler("No active OpenTracing span, can not propagate the baggage items from OpenTracing span context\n")
		return m
	}
	bSpan, ok := span.(*bridgeSpan)
	if !ok {
		t.warningHandler("Encountered a foreign OpenTracing span, will not propagate the baggage items from OpenTracing span context\n")
		return m
	}
	items := bSpan.extraBaggageItems
	if len(items) == 0 {
		return m
	}
	kv := make([]label.KeyValue, 0, len(items))
	for k, v := range items {
		kv = append(kv, label.String(k, v))
	}
	return m.Apply(baggage.MapUpdate{MultiKV: kv})
}

// StartSpan is a part of the implementation of the OpenTracing Tracer
// interface.
func (t *BridgeTracer) StartSpan(operationName string, opts ...ot.StartSpanOption) ot.Span {
	sso := ot.StartSpanOptions{}
	for _, opt := range opts {
		opt.Apply(&sso)
	}
	parentBridgeSC, links := otSpanReferencesToParentAndLinks(sso.References)
	attributes, kind, hadTrueErrorTag := otTagsToOTelAttributesKindAndError(sso.Tags)
	checkCtx := migration.WithDeferredSetup(context.Background())
	if parentBridgeSC != nil {
		checkCtx = otel.ContextWithRemoteSpanReference(checkCtx, parentBridgeSC.otelSpanReference)
	}
	checkCtx2, otelSpan := t.setTracer.tracer().Start(
		checkCtx,
		operationName,
		otel.WithAttributes(attributes...),
		otel.WithTimestamp(sso.StartTime),
		otel.WithLinks(links...),
		otel.WithRecord(),
		otel.WithSpanKind(kind),
	)
	if checkCtx != checkCtx2 {
		t.warnOnce.Do(func() {
			t.warningHandler("SDK should have deferred the context setup, see the documentation of go.opentelemetry.io/otel/bridge/opentracing/migration\n")
		})
	}
	if hadTrueErrorTag {
		otelSpan.SetStatus(codes.Error, "")
	}
	// One does not simply pass a concrete pointer to function
	// that takes some interface. In case of passing nil concrete
	// pointer, we get an interface with non-nil type (because the
	// pointer type is known) and a nil value. Which means
	// interface is not nil, but calling some interface function
	// on it will most likely result in nil pointer dereference.
	var otSpanContext ot.SpanContext
	if parentBridgeSC != nil {
		otSpanContext = parentBridgeSC
	}
	sctx := newBridgeSpanReference(otelSpan.SpanReference(), otSpanContext)
	span := newBridgeSpan(otelSpan, sctx, t)

	return span
}

// ContextWithBridgeSpan sets up the context with the passed
// OpenTelemetry span as the active OpenTracing span.
//
// This function should be used by the OpenTelemetry tracers that want
// to be aware how to operate in the environment using OpenTracing
// API.
func (t *BridgeTracer) ContextWithBridgeSpan(ctx context.Context, span otel.Span) context.Context {
	var otSpanContext ot.SpanContext
	if parentSpan := ot.SpanFromContext(ctx); parentSpan != nil {
		otSpanContext = parentSpan.Context()
	}
	bRef := newBridgeSpanReference(span.SpanReference(), otSpanContext)
	bSpan := newBridgeSpan(span, bRef, t)
	bSpan.skipDeferHook = true
	return ot.ContextWithSpan(ctx, bSpan)
}

// ContextWithSpanHook is an implementation of the OpenTracing tracer
// extension interface. It will call the DeferredContextSetupHook
// function on the tracer if it implements the
// DeferredContextSetupTracerExtension interface.
func (t *BridgeTracer) ContextWithSpanHook(ctx context.Context, span ot.Span) context.Context {
	bSpan, ok := span.(*bridgeSpan)
	if !ok {
		t.warningHandler("Encountered a foreign OpenTracing span, will not run a possible deferred context setup hook\n")
		return ctx
	}
	if bSpan.skipDeferHook {
		return ctx
	}
	if tracerWithExtension, ok := bSpan.tracer.setTracer.tracer().(migration.DeferredContextSetupTracerExtension); ok {
		ctx = tracerWithExtension.DeferredContextSetupHook(ctx, bSpan.otelSpan)
	}
	return ctx
}

func otTagsToOTelAttributesKindAndError(tags map[string]interface{}) ([]label.KeyValue, otel.SpanKind, bool) {
	kind := otel.SpanKindInternal
	err := false
	var pairs []label.KeyValue
	for k, v := range tags {
		switch k {
		case string(otext.SpanKind):
			if s, ok := v.(string); ok {
				switch strings.ToLower(s) {
				case "client":
					kind = otel.SpanKindClient
				case "server":
					kind = otel.SpanKindServer
				case "producer":
					kind = otel.SpanKindProducer
				case "consumer":
					kind = otel.SpanKindConsumer
				}
			}
		case string(otext.Error):
			if b, ok := v.(bool); ok && b {
				err = true
			}
		default:
			pairs = append(pairs, otTagToOTelLabel(k, v))
		}
	}
	return pairs, kind, err
}

func otTagToOTelLabel(k string, v interface{}) label.KeyValue {
	key := otTagToOTelLabelKey(k)
	switch val := v.(type) {
	case bool:
		return key.Bool(val)
	case int64:
		return key.Int64(val)
	case uint64:
		return key.Uint64(val)
	case float64:
		return key.Float64(val)
	case int32:
		return key.Int32(val)
	case uint32:
		return key.Uint32(val)
	case float32:
		return key.Float32(val)
	case int:
		return key.Int(val)
	case uint:
		return key.Uint(val)
	case string:
		return key.String(val)
	default:
		return key.String(fmt.Sprint(v))
	}
}

func otTagToOTelLabelKey(k string) label.Key {
	return label.Key(k)
}

func otSpanReferencesToParentAndLinks(references []ot.SpanReference) (*bridgeSpanReference, []otel.Link) {
	var (
		parent *bridgeSpanReference
		links  []otel.Link
	)
	for _, reference := range references {
		bridgeSC, ok := reference.ReferencedContext.(*bridgeSpanReference)
		if !ok {
			// We ignore foreign ot span contexts,
			// sorry. We have no way of getting any
			// TraceID and SpanID out of it for form a
			// OTel SpanReference for OTel Link. And
			// we can't make it a parent - it also needs a
			// valid OTel SpanReference.
			continue
		}
		if parent != nil {
			links = append(links, otSpanReferenceToOTelLink(bridgeSC, reference.Type))
		} else {
			if reference.Type == ot.ChildOfRef {
				parent = bridgeSC
			} else {
				links = append(links, otSpanReferenceToOTelLink(bridgeSC, reference.Type))
			}
		}
	}
	return parent, links
}

func otSpanReferenceToOTelLink(bridgeSC *bridgeSpanReference, refType ot.SpanReferenceType) otel.Link {
	return otel.Link{
		SpanReference: bridgeSC.otelSpanReference,
		Attributes:    otSpanReferenceTypeToOTelLinkAttributes(refType),
	}
}

func otSpanReferenceTypeToOTelLinkAttributes(refType ot.SpanReferenceType) []label.KeyValue {
	return []label.KeyValue{
		label.String("ot-span-reference-type", otSpanReferenceTypeToString(refType)),
	}
}

func otSpanReferenceTypeToString(refType ot.SpanReferenceType) string {
	switch refType {
	case ot.ChildOfRef:
		// "extra", because first child-of reference is used
		// as a parent, so this function isn't even called for
		// it.
		return "extra-child-of"
	case ot.FollowsFromRef:
		return "follows-from-ref"
	default:
		return fmt.Sprintf("unknown-%d", int(refType))
	}
}

// fakeSpan is just a holder of span reference, nothing more. It's for
// propagators, so they can get the span reference from Go context.
type fakeSpan struct {
	otel.Span
	sc otel.SpanReference
}

func (s fakeSpan) SpanReference() otel.SpanReference {
	return s.sc
}

// Inject is a part of the implementation of the OpenTracing Tracer
// interface.
//
// Currently only the HTTPHeaders format is supported.
func (t *BridgeTracer) Inject(sm ot.SpanContext, format interface{}, carrier interface{}) error {
	bridgeSC, ok := sm.(*bridgeSpanReference)
	if !ok {
		return ot.ErrInvalidSpanContext
	}
	if !bridgeSC.otelSpanReference.IsValid() {
		return ot.ErrInvalidSpanContext
	}
	if builtinFormat, ok := format.(ot.BuiltinFormat); !ok || builtinFormat != ot.HTTPHeaders {
		return ot.ErrUnsupportedFormat
	}
	hhcarrier, ok := carrier.(ot.HTTPHeadersCarrier)
	if !ok {
		return ot.ErrInvalidCarrier
	}
	header := http.Header(hhcarrier)
	fs := fakeSpan{
		Span: noop.Span,
		sc:   bridgeSC.otelSpanReference,
	}
	ctx := otel.ContextWithSpan(context.Background(), fs)
	ctx = baggage.ContextWithMap(ctx, bridgeSC.baggageItems)
	t.getPropagator().Inject(ctx, header)
	return nil
}

// Extract is a part of the implementation of the OpenTracing Tracer
// interface.
//
// Currently only the HTTPHeaders format is supported.
func (t *BridgeTracer) Extract(format interface{}, carrier interface{}) (ot.SpanContext, error) {
	if builtinFormat, ok := format.(ot.BuiltinFormat); !ok || builtinFormat != ot.HTTPHeaders {
		return nil, ot.ErrUnsupportedFormat
	}
	hhcarrier, ok := carrier.(ot.HTTPHeadersCarrier)
	if !ok {
		return nil, ot.ErrInvalidCarrier
	}
	header := http.Header(hhcarrier)
	ctx := t.getPropagator().Extract(context.Background(), header)
	baggage := baggage.MapFromContext(ctx)
	otelSC, _, _ := otelparent.GetSpanReferenceAndLinks(ctx, false)
	bridgeSC := &bridgeSpanReference{
		baggageItems:      baggage,
		otelSpanReference: otelSC,
	}
	if !bridgeSC.otelSpanReference.IsValid() {
		return nil, ot.ErrSpanContextNotFound
	}
	return bridgeSC, nil
}

func (t *BridgeTracer) getPropagator() otel.TextMapPropagator {
	if t.propagator != nil {
		return t.propagator
	}
	return otelglobal.TextMapPropagator()
}
