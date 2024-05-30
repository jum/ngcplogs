package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/api/option"

	"github.com/docker/docker/daemon/logger"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/containerd/log"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

const (
	name = "ngcplogs"

	projectOptKey         = "gcp-project"
	logLabelsKey          = "labels"
	logLabelsRegexKey     = "labels-regex"
	logEnvKey             = "env"
	logEnvRegexKey        = "env-regex"
	logCmdKey             = "gcp-log-cmd"
	logZoneKey            = "gcp-meta-zone"
	logNameKey            = "gcp-meta-name"
	logIDKey              = "gcp-meta-id"
	clientCredentialsFile = "credentials-file"
	clientCredentialsJSON = "credentials-json"
)

var (
	// The number of logs the gcplogs driver has dropped.
	droppedLogs uint64

	onGCE bool

	// instance metadata populated from the metadata server if available
	projectID    string
	zone         string
	instanceName string
	instanceID   string

	severityFields = []string{
		"severity",
		"level",
	}

	timestampFields = []string{
		"timestamp",
		"time",
		"ts",
	}
)

func init() {
	if err := logger.RegisterLogDriver(name, New); err != nil {
		panic(err)
	}

	if err := logger.RegisterLogOptValidator(name, ValidateLogOpts); err != nil {
		panic(err)
	}
}

type nGCPLogger struct {
	client    *logging.Client
	logger    *logging.Logger
	instance  *instanceInfo
	container *containerInfo
	projectID string

	extractJsonMessage bool
	extractSeverity    bool
	excludeTimestamp   bool
	extractMsg         bool
	extractGcp         bool
	extractCaddy       bool
}

type dockerLogEntry struct {
	Instance  *instanceInfo  `json:"instance,omitempty"`
	Container *containerInfo `json:"container,omitempty"`
	Message   string         `json:"message,omitempty"`
}

type instanceInfo struct {
	Zone string `json:"zone,omitempty"`
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

type containerInfo struct {
	Name      string            `json:"name,omitempty"`
	ID        string            `json:"id,omitempty"`
	ImageName string            `json:"imageName,omitempty"`
	ImageID   string            `json:"imageId,omitempty"`
	Created   time.Time         `json:"created,omitempty"`
	Command   string            `json:"command,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

var initGCPOnce sync.Once

func initGCP() {
	initGCPOnce.Do(func() {
		onGCE = metadata.OnGCE()
		if onGCE {
			// These will fail on instances if the metadata service is
			// down or the client is compiled with an API version that
			// has been removed. Since these are not vital, let's ignore
			// them and make their fields in the dockerLogEntry ,omitempty
			projectID, _ = metadata.ProjectID()
			zone, _ = metadata.Zone()
			instanceName, _ = metadata.InstanceName()
			instanceID, _ = metadata.InstanceID()
		}
	})
}

// New creates a new logger that logs to Google Cloud Logging using the application
// default credentials.
//
// See https://developers.google.com/identity/protocols/application-default-credentials
func New(info logger.Info) (logger.Logger, error) {
	initGCP()

	var project string
	if projectID != "" {
		project = projectID
	}
	if projectID, found := info.Config[projectOptKey]; found {
		project = projectID
	}
	if project == "" {
		return nil, fmt.Errorf("no project was specified and couldn't read project from the metadata server. Please specify a project")
	}

	var opts []option.ClientOption
	if credentialsFile, found := info.Config[clientCredentialsFile]; found {
		opts = append(opts, option.WithCredentialsFile(fmt.Sprintf("/host/%s", credentialsFile)))
	} else if credentialsJSON, found := info.Config[clientCredentialsJSON]; found {
		opts = append(opts, option.WithCredentialsJSON([]byte(credentialsJSON)))
	}

	c, err := logging.NewClient(context.Background(), project, opts...)
	if err != nil {
		return nil, err
	}
	var instanceResource *instanceInfo
	if onGCE {
		instanceResource = &instanceInfo{
			Zone: zone,
			Name: instanceName,
			ID:   instanceID,
		}
	} else if info.Config[logZoneKey] != "" || info.Config[logNameKey] != "" || info.Config[logIDKey] != "" {
		instanceResource = &instanceInfo{
			Zone: info.Config[logZoneKey],
			Name: info.Config[logNameKey],
			ID:   info.Config[logIDKey],
		}
	}

	var options []logging.LoggerOption
	if instanceResource != nil {
		vmMrpb := logging.CommonResource(
			&mrpb.MonitoredResource{
				Type: "gce_instance",
				Labels: map[string]string{
					"instance_id": instanceResource.ID,
					"zone":        instanceResource.Zone,
				},
			},
		)
		options = []logging.LoggerOption{vmMrpb}
	}
	lg := c.Logger("ngcplogs-docker-driver", options...)

	if err := c.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("unable to connect or authenticate with Google Cloud Logging: %v", err)
	}

	extraAttributes, err := info.ExtraAttributes(nil)
	if err != nil {
		return nil, err
	}

	l := &nGCPLogger{
		client: c,
		logger: lg,
		container: &containerInfo{
			Name:      info.ContainerName,
			ID:        info.ContainerID,
			ImageName: info.ContainerImageName,
			ImageID:   info.ContainerImageID,
			Created:   info.ContainerCreated,
			Metadata:  extraAttributes,
		},
		projectID:          project,
		extractJsonMessage: true,
		extractSeverity:    true,
		excludeTimestamp:   false,
		extractMsg:         true,
		extractGcp:         false,
		extractCaddy:       false,
	}

	if info.Config[logCmdKey] == "true" {
		l.container.Command = info.Command()
	}

	if info.Config["extract-json-message"] == "false" {
		l.extractJsonMessage = false
	}
	if info.Config["extract-severity"] == "false" {
		l.extractSeverity = false
	}
	if info.Config["exclude-timestamp"] == "true" {
		l.excludeTimestamp = true
	}
	if info.Config["extract-msg"] == "false" {
		l.extractMsg = false
	}
	if info.Config["extract-gcp"] == "true" {
		l.extractGcp = true
	}
	if info.Config["extract-caddy"] == "true" {
		l.extractCaddy = true
	}

	if instanceResource != nil {
		l.instance = instanceResource
	}

	// The logger "overflows" at a rate of 10,000 logs per second and this
	// overflow func is called. We want to surface the error to the user
	// without overly spamming /var/log/docker.log so we log the first time
	// we overflow and every 1000th time after.
	c.OnError = func(err error) {
		if errors.Is(err, logging.ErrOverflow) {
			if i := atomic.AddUint64(&droppedLogs, 1); i%1000 == 1 {
				log.G(context.TODO()).Errorf("ngcplogs driver has dropped %v logs", i)
			}
		} else {
			log.G(context.TODO()).Error(err)
		}
	}

	return l, nil
}

// ValidateLogOpts validates the opts passed to the ngcplogs driver. Currently, the ngcplogs
// driver doesn't take any arguments.
func ValidateLogOpts(cfg map[string]string) error {
	for k := range cfg {
		switch k {
		case projectOptKey, logLabelsKey, logLabelsRegexKey, logEnvKey, logEnvRegexKey, logCmdKey, logZoneKey, logNameKey, logIDKey:
		default:
			return fmt.Errorf("%q is not a valid option for the ngcplogs driver", k)
		}
	}
	return nil
}

func (l *nGCPLogger) Log(lMsg *logger.Message) error {
	logLine := lMsg.Line
	ts := lMsg.Timestamp
	logger.PutMessage(lMsg)

	if len(logLine) > 0 {
		var payload any
		entry := logging.Entry{
			Labels:    map[string]string{},
			Timestamp: ts,
			Severity:  logging.Default,
		}

		if l.extractJsonMessage && logLine[0] == '{' && logLine[len(logLine)-1] == '}' {
			var m map[string]any
			err := json.Unmarshal(logLine, &m)
			if err != nil {
				payload = fmt.Sprintf("Error parsing JSON: %s", string(logLine))
				entry.Severity = logging.Critical
			} else {
				entry.Severity = l.extractSeverityFromPayload(m)
				l.excludeTimestampFromPayload(m)
				l.extractMsgFromPayload(m)
				m["instance"] = l.instance
				m["container"] = l.container
				l.extractGcpFromPayload(m, &entry)
				l.extractCaddyFromPayload(m, &entry)
				payload = m
			}
		} else {
			payload = dockerLogEntry{
				Instance:  l.instance,
				Container: l.container,
				Message:   string(logLine),
			}
		}

		entry.Payload = payload
		l.logger.Log(entry)
	}
	return nil
}

func (l *nGCPLogger) extractSeverityFromPayload(m map[string]any) logging.Severity {
	severity := logging.Default

	if l.extractSeverity {
		for _, severityField := range severityFields {
			if rawSeverity, exists := m[severityField]; exists {
				if parsedSeverity, isString := rawSeverity.(string); isString {
					severity = logging.ParseSeverity(parsedSeverity)
					if severity != logging.Default { // severity was parsed correctly, we can remove it from the jsonPayload section
						delete(m, severityField)
					}
					break
				}
				if parsedSeverity, isNumber := rawSeverity.(float64); isNumber {
					severity = logging.Severity(parsedSeverity)
					if severity != logging.Default { // severity was parsed correctly, we can remove it from the jsonPayload section
						delete(m, severityField)
					}
					break
				}
			}
		}
	}

	return severity
}

func (l *nGCPLogger) excludeTimestampFromPayload(m map[string]any) {
	if l.excludeTimestamp {
		for _, timestampField := range timestampFields {
			if _, exists := m[timestampField]; exists {
				delete(m, timestampField)
			}
		}
	}
}

func (l *nGCPLogger) extractMsgFromPayload(m map[string]any) {

	if l.extractMsg {
		if msg, exists := m["msg"]; exists {
			m["message"] = msg
			delete(m, "msg")
		}
	}
}

func assertOrLog[T any](val any) T {
	var v T
	var ok bool
	v, ok = val.(T)
	if !ok {
		_, file, line, ok := runtime.Caller(1)
		if !ok {
			file = "unknown"
		}
		slog.Error("unexpected type", "want", reflect.TypeOf(v).String(), "got", reflect.TypeOf(val).String(), "file", file, "line", line)
	}
	return v
}

func (l *nGCPLogger) extractGcpFromPayload(m map[string]any, entry *logging.Entry) {

	if l.extractGcp {
		if val, exists := m["logging.googleapis.com/sourceLocation"]; exists {
			v := assertOrLog[map[string]any](val)
			entry.SourceLocation = &loggingpb.LogEntrySourceLocation{
				File:     assertOrLog[string](v["file"]),
				Line:     int64(assertOrLog[float64](v["line"])),
				Function: assertOrLog[string](v["function"]),
			}
			delete(m, "logging.googleapis.com/sourceLocation")
		}
		if val, exists := m["logging.googleapis.com/trace"]; exists {
			entry.Trace = assertOrLog[string](val)
			delete(m, "logging.googleapis.com/trace")
		}
		if val, exists := m["logging.googleapis.com/spanId"]; exists {
			entry.SpanID = assertOrLog[string](val)
			delete(m, "logging.googleapis.com/spanId")
		}
		if val, exists := m["logging.googleapis.com/trace_sampled"]; exists {
			entry.TraceSampled = assertOrLog[bool](val)
			delete(m, "logging.googleapis.com/trace_sampled")
		}
		if val, exists := m["logging.googleapis.com/labels"]; exists {
			v := assertOrLog[map[string]any](val)
			for k, v := range v {
				entry.Labels[k] = assertOrLog[string](v)
			}
			delete(m, "logging.googleapis.com/labels")
		}
	}
}

/*
 * this comment is for debugging build problems.
 */
func (l *nGCPLogger) extractCaddyFromPayload(m map[string]any, entry *logging.Entry) {

	if l.extractCaddy {
		if val, exists := m["request"]; exists {
			hr := logging.HTTPRequest{
				Request: &http.Request{
					Header: make(http.Header),
				},
			}
			v := assertOrLog[map[string]any](val)
			hr.Request.Method = assertOrLog[string](v["method"])
			_, isTLS := v["tls"]
			var h = "http"
			if isTLS {
				h = "https"
			}
			hr.Request.URL = &url.URL{
				Scheme:  h,
				Host:    assertOrLog[string](v["host"]),
				RawPath: assertOrLog[string](v["uri"]),
				Path:    assertOrLog[string](v["uri"]),
			}
			if t, ok := m["bytes_read"]; ok {
				hr.RequestSize = int64(assertOrLog[float64](t))
			}
			if t, ok := m["status"]; ok {
				hr.Status = int(assertOrLog[float64](t))
			}
			if t, ok := m["size"]; ok {
				hr.ResponseSize = int64(assertOrLog[float64](t))
			}
			if t, ok := m["duration"]; ok {
				hr.Latency = time.Duration(assertOrLog[float64](t) * float64(time.Second))
			}
			hr.Request.Proto = assertOrLog[string](v["proto"])
			hr.RemoteIP = assertOrLog[string](v["remote_ip"]) + ":" + assertOrLog[string](v["remote_port"])

			if t, ok := v["headers"]; ok {
				headers := assertOrLog[map[string]any](t)
				for h, v := range headers {
					for _, s := range assertOrLog[[]any](v) {
						hr.Request.Header.Add(h, assertOrLog[string](s))
					}
				}
			}
			entry.HTTPRequest = &hr
			//Caddy request contains more data, don't
			//delete.
			//delete(m, "request")
		}
		if val, exists := m["traceID"]; exists {
			entry.Trace = "projects/" + l.projectID + "/traces/" + val.(string)
			delete(m, "traceID")
		}
	}
}

func (l *nGCPLogger) Close() error {
	err := l.logger.Flush()
	if err != nil {
		return err
	}
	return l.client.Close()
}

func (l *nGCPLogger) Name() string {
	return name
}
