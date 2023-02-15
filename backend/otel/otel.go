package otel

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi"
	"github.com/highlight-run/highlight/backend/clickhouse"
	kafkaqueue "github.com/highlight-run/highlight/backend/kafka-queue"
	model2 "github.com/highlight-run/highlight/backend/model"
	"github.com/highlight-run/highlight/backend/public-graph/graph"
	"github.com/highlight-run/highlight/backend/public-graph/graph/model"
	"github.com/highlight/highlight/sdk/highlight-go"
	hlog "github.com/highlight/highlight/sdk/highlight-go/log"
	"github.com/openlyinc/pointy"
	e "github.com/pkg/errors"
	"github.com/samber/lo"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type Handler struct {
	resolver *graph.Resolver
}

func castString(v interface{}, fallback string) string {
	s, _ := v.(string)
	if s == "" {
		return fallback
	}
	return s
}

func setHighlightAttributes(attrs map[string]any, projectID, sessionID, requestID *string) {
	if p, ok := attrs[highlight.ProjectIDAttribute]; ok {
		if p.(string) != "" {
			*projectID = p.(string)
		}
	}
	if s, ok := attrs[highlight.SessionIDAttribute]; ok {
		if s.(string) != "" {
			*sessionID = s.(string)
		}
	}
	if r, ok := attrs[highlight.RequestIDAttribute]; ok {
		if r.(string) != "" {
			*requestID = r.(string)
		}
	}
}

func projectToInt(projectID string) (int, error) {
	i, err := strconv.ParseInt(projectID, 10, 32)
	if err == nil {
		return int(i), nil
	}
	i2, err := model2.FromVerboseID(projectID)
	if err == nil {
		return i2, nil
	}
	return 0, e.New(fmt.Sprintf("invalid project id %s", projectID))
}

func (o *Handler) HandleTrace(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error(err, "invalid trace body")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		log.Error(err, "invalid gzip format for trace")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	output, err := io.ReadAll(gz)
	if err != nil {
		log.Error(err, "invalid gzip stream for trace")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req := ptraceotlp.NewExportRequest()
	err = req.UnmarshalProto(output)
	if err != nil {
		log.Error(err, "invalid trace protobuf")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var projectErrors = make(map[string][]*model.BackendErrorObjectInput)
	var traceErrors = make(map[string][]*model.BackendErrorObjectInput)

	var projectLogs = make(map[string][]*clickhouse.LogRow)

	spans := req.Traces().ResourceSpans()
	for i := 0; i < spans.Len(); i++ {
		var projectID, sessionID, requestID string
		resource := spans.At(i).Resource()
		resourceAttributes := resource.Attributes().AsRaw()
		sdkLanguage := castString(resource.Attributes().AsRaw()[string(semconv.TelemetrySDKLanguageKey)], "")
		serviceName := castString(resource.Attributes().AsRaw()[string(semconv.ServiceNameKey)], "")
		setHighlightAttributes(resourceAttributes, &projectID, &sessionID, &requestID)
		scopeScans := spans.At(i).ScopeSpans()
		for j := 0; j < scopeScans.Len(); j++ {
			scope := scopeScans.At(j).Scope()
			spans := scopeScans.At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)
				spanAttributes := span.Attributes().AsRaw()
				tagsBytes, err := json.Marshal(spanAttributes)
				if err != nil {
					log.Errorf("failed to format error attributes %s", tagsBytes)
					continue
				}
				setHighlightAttributes(spanAttributes, &projectID, &sessionID, &requestID)
				events := span.Events()
				for l := 0; l < events.Len(); l++ {
					event := events.At(l)
					eventAttributes := event.Attributes().AsRaw()
					setHighlightAttributes(eventAttributes, &projectID, &sessionID, &requestID)
					if event.Name() == semconv.ExceptionEventName {
						excType := castString(eventAttributes[string(semconv.ExceptionTypeKey)], "")
						excMessage := castString(eventAttributes[string(semconv.ExceptionMessageKey)], "")
						stackTrace := castString(eventAttributes[string(semconv.ExceptionStacktraceKey)], "")
						errorUrl := castString(eventAttributes[highlight.ErrorURLKey], "")
						if stackTrace == "" {
							log.WithField("Span", span).WithField("EventAttributes", eventAttributes).Warn("otel received exception with no stacktrace")
							continue
						} else if excType == "" && excMessage == "" {
							log.WithField("Span", span).WithField("EventAttributes", eventAttributes).Warn("otel received exception with no type and no message")
							continue
						}
						stackTrace = formatStructureStackTrace(stackTrace)
						err := &model.BackendErrorObjectInput{
							SessionSecureID: &sessionID,
							RequestID:       &requestID,
							TraceID:         pointy.String(span.TraceID().String()),
							SpanID:          pointy.String(span.SpanID().String()),
							Event:           castString(excMessage, ""),
							Type:            castString(excType, "BACKEND"),
							Source: strings.Join(lo.Filter([]string{
								sdkLanguage,
								serviceName,
								scope.Name(),
							}, func(s string, i int) bool {
								return s != ""
							}), "-"),
							StackTrace: stackTrace,
							Timestamp:  event.Timestamp().AsTime(),
							Payload:    pointy.String(string(tagsBytes)),
							URL:        errorUrl,
						}
						if sessionID != "" {
							if _, ok := traceErrors[sessionID]; !ok {
								traceErrors[sessionID] = []*model.BackendErrorObjectInput{}
							}
							traceErrors[sessionID] = append(traceErrors[sessionID], err)
						} else if projectID != "" {
							if _, ok := projectErrors[projectID]; !ok {
								projectErrors[projectID] = []*model.BackendErrorObjectInput{}
							}
							projectErrors[projectID] = append(projectErrors[projectID], err)
						} else {
							data, _ := req.MarshalJSON()
							log.WithField("BackendErrorObjectInput", *err).WithField("RequestJSON", string(data)).Errorf("otel error got no session and no project")
							continue
						}
					} else if event.Name() == hlog.LogName {
						logSev := castString(eventAttributes[string(hlog.LogSeverityKey)], "unknown")
						logMessage := castString(eventAttributes[string(hlog.LogMessageKey)], "")
						if logMessage == "" {
							log.WithField("Span", span).WithField("EventAttributes", eventAttributes).Warn("otel received log with no message")
							continue
						}
						projectIDInt, err := projectToInt(projectID)
						if err != nil {
							log.WithField("ProjectVerboseID", projectID).WithField("LogMessage", logMessage).Errorf("otel span log got invalid project id")
							continue
						}
						resourceAttributesMap := make(map[string]string)
						for k, v := range resourceAttributes {
							vStr := castString(v, "")
							if vStr != "" {
								resourceAttributesMap[k] = castString(v, "")
							}
						}
						logAttributesMap := make(map[string]string)
						for k, v := range eventAttributes {
							vStr := castString(v, "")
							if vStr == "" || k == string(hlog.LogMessageKey) || k == string(hlog.LogSeverityKey) {
								continue
							}
							logAttributesMap[k] = castString(v, "")
						}
						lvl, _ := log.ParseLevel(logSev)
						logRow := &clickhouse.LogRow{
							Timestamp:          event.Timestamp().AsTime(),
							TraceId:            span.TraceID().String(),
							SpanId:             span.SpanID().String(),
							SeverityText:       logSev,
							SeverityNumber:     int32(lvl),
							ServiceName:        serviceName,
							Body:               logMessage,
							ResourceAttributes: resourceAttributesMap,
							LogAttributes:      logAttributesMap,
							ProjectId:          uint32(projectIDInt),
							SecureSessionId:    sessionID,
						}
						if projectID != "" {
							if _, ok := projectLogs[projectID]; !ok {
								projectLogs[projectID] = []*clickhouse.LogRow{}
							}
							projectLogs[projectID] = append(projectLogs[projectID], logRow)
						} else {
							data, _ := req.MarshalJSON()
							log.WithField("LogEvent", event).WithField("LogRow", *logRow).WithField("RequestJSON", string(data)).Errorf("otel span log got no project")
							continue
						}
					}
				}
			}
		}
	}

	for sessionID, errors := range traceErrors {
		err = o.resolver.ProducerQueue.Submit(&kafkaqueue.Message{
			Type: kafkaqueue.PushBackendPayload,
			PushBackendPayload: &kafkaqueue.PushBackendPayloadArgs{
				SessionSecureID: &sessionID,
				Errors:          errors,
			}}, sessionID)
		if err != nil {
			log.Error(err, "failed to submit otel session errors to public worker queue")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}

	for projectID, errors := range projectErrors {
		err = o.resolver.ProducerQueue.Submit(&kafkaqueue.Message{
			Type: kafkaqueue.PushBackendPayload,
			PushBackendPayload: &kafkaqueue.PushBackendPayloadArgs{
				ProjectVerboseID: &projectID,
				Errors:           errors,
			}}, projectID)
		if err != nil {
			log.Error(err, "failed to submit otel project errors to public worker queue")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}

	for projectID, logRows := range projectLogs {
		err = o.resolver.BatchedQueue.Submit(&kafkaqueue.Message{
			Type: kafkaqueue.PushLogs,
			PushLogs: &kafkaqueue.PushLogsArgs{
				LogRows: logRows,
			}}, projectID)
		if err != nil {
			log.Error(err, "failed to submit otel project errors to public worker queue")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (o *Handler) HandleLog(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error(err, "invalid log body")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		log.Error(err, "invalid gzip format for log")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	output, err := io.ReadAll(gz)
	if err != nil {
		log.Error(err, "invalid gzip stream for log")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req := plogotlp.NewExportRequest()
	err = req.UnmarshalProto(output)
	if err != nil {
		log.Error(err, "invalid log protobuf")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var projectLogs = make(map[string][]*clickhouse.LogRow)

	resourceLogs := req.Logs().ResourceLogs()
	for i := 0; i < resourceLogs.Len(); i++ {
		var projectID, sessionID, requestID string
		resource := resourceLogs.At(i).Resource()
		resourceAttributes := resource.Attributes().AsRaw()
		serviceName := castString(resource.Attributes().AsRaw()[string(semconv.ServiceNameKey)], "")
		setHighlightAttributes(resourceAttributes, &projectID, &sessionID, &requestID)
		scopeLogs := resourceLogs.At(i).ScopeLogs()
		for j := 0; j < scopeLogs.Len(); j++ {
			logRecords := scopeLogs.At(j).LogRecords()
			for k := 0; k < logRecords.Len(); k++ {
				logRecord := logRecords.At(k)
				logAttributes := logRecord.Attributes().AsRaw()
				setHighlightAttributes(logAttributes, &projectID, &sessionID, &requestID)
				projectIDInt, err := projectToInt(projectID)
				if err != nil {
					log.WithField("ProjectID", projectID).WithField("LogMessage", logRecord.Body().AsRaw()).Errorf("otel log got invalid project id")
					continue
				}
				resourceAttributesMap := make(map[string]string)
				for k, v := range resourceAttributes {
					vStr := castString(v, "")
					if vStr != "" {
						resourceAttributesMap[k] = castString(v, "")
					}
				}
				logAttributesMap := make(map[string]string)
				for k, v := range logAttributes {
					vStr := castString(v, "")
					if vStr != "" {
						logAttributesMap[k] = castString(v, "")
					}
				}
				logRow := &clickhouse.LogRow{
					Timestamp:          logRecord.Timestamp().AsTime(),
					TraceId:            logRecord.TraceID().String(),
					SpanId:             logRecord.SpanID().String(),
					SeverityText:       logRecord.SeverityText(),
					SeverityNumber:     int32(logRecord.SeverityNumber()),
					ServiceName:        serviceName,
					Body:               logRecord.Body().Str(),
					ResourceAttributes: resourceAttributesMap,
					LogAttributes:      logAttributesMap,
					ProjectId:          uint32(projectIDInt),
					SecureSessionId:    sessionID,
				}
				if projectID != "" {
					if _, ok := projectLogs[projectID]; !ok {
						projectLogs[projectID] = []*clickhouse.LogRow{}
					}
					projectLogs[projectID] = append(projectLogs[projectID], logRow)
				} else {
					data, _ := req.MarshalJSON()
					log.WithField("LogRecord", logRecords).WithField("LogRow", *logRow).WithField("RequestJSON", string(data)).Errorf("otel log got no project")
					continue
				}
			}
		}
	}

	for projectID, logRows := range projectLogs {
		err = o.resolver.BatchedQueue.Submit(&kafkaqueue.Message{
			Type: kafkaqueue.PushLogs,
			PushLogs: &kafkaqueue.PushLogsArgs{
				LogRows: logRows,
			}}, projectID)
		if err != nil {
			log.Error(err, "failed to submit otel project errors to public worker queue")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (o *Handler) Listen(r *chi.Mux) {
	r.Route("/otel/v1", func(r chi.Router) {
		r.HandleFunc("/traces", o.HandleTrace)
		r.HandleFunc("/logs", o.HandleLog)
	})
}

func New(resolver *graph.Resolver) *Handler {
	return &Handler{
		resolver: resolver,
	}
}