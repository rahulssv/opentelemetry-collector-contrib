// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package syslog // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/parser/syslog"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sl "github.com/haimrubinstein/go-syslog/v3"
	"github.com/haimrubinstein/go-syslog/v3/nontransparent"
	"github.com/haimrubinstein/go-syslog/v3/octetcounting"
	"github.com/haimrubinstein/go-syslog/v3/rfc3164"
	"github.com/haimrubinstein/go-syslog/v3/rfc5424"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/entry"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/helper"
)

const (
	operatorType = "syslog_parser"

	RFC3164 = "rfc3164"
	RFC5424 = "rfc5424"

	NULTrailer = "NUL"
	LFTrailer  = "LF"
)

func init() {
	operator.Register(operatorType, func() operator.Builder { return NewConfig() })
}

// NewConfig creates a new syslog parser config with default values
func NewConfig() *Config {
	return NewConfigWithID(operatorType)
}

// NewConfigWithID creates a new syslog parser config with default values
func NewConfigWithID(operatorID string) *Config {
	return &Config{
		ParserConfig: helper.NewParserConfig(operatorID, operatorType),
	}
}

// Config is the configuration of a syslog parser operator.
type Config struct {
	helper.ParserConfig `mapstructure:",squash"`
	BaseConfig          `mapstructure:",squash"`
}

// BaseConfig is the detailed configuration of a syslog parser.
type BaseConfig struct {
	Protocol                     string  `mapstructure:"protocol,omitempty"`
	Location                     string  `mapstructure:"location,omitempty"`
	EnableOctetCounting          bool    `mapstructure:"enable_octet_counting,omitempty"`
	AllowSkipPriHeader           bool    `mapstructure:"allow_skip_pri_header,omitempty"`
	NonTransparentFramingTrailer *string `mapstructure:"non_transparent_framing_trailer,omitempty"`
}

// Build will build a JSON parser operator.
func (c Config) Build(logger *zap.SugaredLogger) (operator.Operator, error) {
	if c.ParserConfig.TimeParser == nil {
		parseFromField := entry.NewAttributeField("timestamp")
		c.ParserConfig.TimeParser = &helper.TimeParser{
			ParseFrom:  &parseFromField,
			LayoutType: helper.NativeKey,
		}
	}

	parserOperator, err := c.ParserConfig.Build(logger)
	if err != nil {
		return nil, err
	}

	proto := strings.ToLower(c.Protocol)

	switch {
	case proto == "":
		return nil, fmt.Errorf("missing field 'protocol'")
	case proto != RFC5424 && (c.NonTransparentFramingTrailer != nil || c.EnableOctetCounting):
		return nil, errors.New("octet_counting and non_transparent_framing are only compatible with protocol rfc5424")
	case proto == RFC5424 && (c.NonTransparentFramingTrailer != nil && c.EnableOctetCounting):
		return nil, errors.New("only one of octet_counting or non_transparent_framing can be enabled")
	case proto == RFC5424 && c.NonTransparentFramingTrailer != nil:
		if *c.NonTransparentFramingTrailer != NULTrailer && *c.NonTransparentFramingTrailer != LFTrailer {
			return nil, fmt.Errorf("invalid non_transparent_framing_trailer '%s'. Must be either 'LF' or 'NUL'", *c.NonTransparentFramingTrailer)
		}
	case proto != RFC5424 && proto != RFC3164:
		return nil, fmt.Errorf("unsupported protocol version: %s", proto)
	}

	if c.Location == "" {
		c.Location = "UTC"
	}

	location, err := time.LoadLocation(c.Location)
	if err != nil {
		return nil, fmt.Errorf("failed to load location %s: %w", c.Location, err)
	}

	return &Parser{
		ParserOperator:               parserOperator,
		protocol:                     proto,
		location:                     location,
		enableOctetCounting:          c.EnableOctetCounting,
		allowSkipPriHeader:           c.AllowSkipPriHeader,
		nonTransparentFramingTrailer: c.NonTransparentFramingTrailer,
	}, nil
}

// parseFunc a parseFunc determines how the raw input is to be parsed into a syslog message
type parseFunc func(input []byte) (sl.Message, error)

func (s *Parser) buildParseFunc() (parseFunc, error) {
	switch s.protocol {
	case RFC3164:
		return func(input []byte) (sl.Message, error) {
			parserOptions := []sl.MachineOption{rfc3164.WithLocaleTimezone(s.location)}
			if s.allowSkipPriHeader {
				parserOptions = append(parserOptions, rfc3164.WithAllowSkipPri())
			}
			return rfc3164.NewMachine(parserOptions...).Parse(input)
		}, nil
	case RFC5424:
		switch {
		// Octet Counting Parsing RFC6587
		case s.enableOctetCounting:
			return newOctetCountingParseFunc(), nil
		// Non-Transparent-Framing Parsing RFC6587
		case s.nonTransparentFramingTrailer != nil && *s.nonTransparentFramingTrailer == LFTrailer:
			return newNonTransparentFramingParseFunc(nontransparent.LF), nil
		case s.nonTransparentFramingTrailer != nil && *s.nonTransparentFramingTrailer == NULTrailer:
			return newNonTransparentFramingParseFunc(nontransparent.NUL), nil
		// Raw RFC5424 parsing
		default:
			return func(input []byte) (sl.Message, error) {
				parserOptions := []sl.MachineOption{}
				if s.allowSkipPriHeader {
					parserOptions = append(parserOptions, rfc5424.WithAllowSkipPri())
				}
				return rfc5424.NewMachine(parserOptions...).Parse(input)
			}, nil
		}

	default:
		return nil, fmt.Errorf("invalid protocol %s", s.protocol)
	}
}

// Parser is an operator that parses syslog.
type Parser struct {
	helper.ParserOperator
	protocol                     string
	location                     *time.Location
	enableOctetCounting          bool
	allowSkipPriHeader           bool
	nonTransparentFramingTrailer *string
}

// Process will parse an entry field as syslog.
func (s *Parser) Process(ctx context.Context, entry *entry.Entry) error {

	// if pri header is missing and this is an expected behavior then facility and severity values should be skipped.
	if !s.enableOctetCounting && s.allowSkipPriHeader {

		bytes, err := toBytes(entry.Body)
		if err != nil {
			return err
		}

		if s.shouldSkipPriorityValues(bytes) {
			return s.ParserOperator.ProcessWithCallback(ctx, entry, s.parse, postprocessWithoutPriHeader)
		}
	}

	return s.ParserOperator.ProcessWithCallback(ctx, entry, s.parse, postprocess)
}

// parse will parse a value as syslog.
func (s *Parser) parse(value any) (any, error) {
	bytes, err := toBytes(value)
	if err != nil {
		return nil, err
	}

	pFunc, err := s.buildParseFunc()
	if err != nil {
		return nil, err
	}

	slog, err := pFunc(bytes)
	if err != nil {
		return nil, err
	}

	skipPriHeaderValues := s.shouldSkipPriorityValues(bytes)

	switch message := slog.(type) {
	case *rfc3164.SyslogMessage:
		return s.parseRFC3164(message, skipPriHeaderValues)
	case *rfc5424.SyslogMessage:
		return s.parseRFC5424(message, skipPriHeaderValues)
	default:
		return nil, fmt.Errorf("parsed value was not rfc3164 or rfc5424 compliant")
	}
}

func (s *Parser) shouldSkipPriorityValues(value []byte) bool {
	if !s.enableOctetCounting && s.allowSkipPriHeader {
		// check if entry starts with '<'.
		// if not it means that the pre header was missing from the body and hence we should skip it.
		if len(value) > 1 && value[0] != '<' {
			return true
		}
	}
	return false
}

// parseRFC3164 will parse an RFC3164 syslog message.
func (s *Parser) parseRFC3164(syslogMessage *rfc3164.SyslogMessage, skipPriHeaderValues bool) (map[string]any, error) {
	value := map[string]any{
		"timestamp": syslogMessage.Timestamp,
		"hostname":  syslogMessage.Hostname,
		"appname":   syslogMessage.Appname,
		"proc_id":   syslogMessage.ProcID,
		"msg_id":    syslogMessage.MsgID,
		"message":   syslogMessage.Message,
	}

	if !skipPriHeaderValues {
		value["priority"] = syslogMessage.Priority
		value["severity"] = syslogMessage.Severity
		value["facility"] = syslogMessage.Facility
	}

	return s.toSafeMap(value)
}

// parseRFC5424 will parse an RFC5424 syslog message.
func (s *Parser) parseRFC5424(syslogMessage *rfc5424.SyslogMessage, skipPriHeaderValues bool) (map[string]any, error) {
	value := map[string]any{
		"timestamp":       syslogMessage.Timestamp,
		"hostname":        syslogMessage.Hostname,
		"appname":         syslogMessage.Appname,
		"proc_id":         syslogMessage.ProcID,
		"msg_id":          syslogMessage.MsgID,
		"message":         syslogMessage.Message,
		"structured_data": syslogMessage.StructuredData,
		"version":         syslogMessage.Version,
	}

	if !skipPriHeaderValues {
		value["priority"] = syslogMessage.Priority
		value["severity"] = syslogMessage.Severity
		value["facility"] = syslogMessage.Facility
	}

	return s.toSafeMap(value)
}

// toSafeMap will dereference any pointers on the supplied map.
func (s *Parser) toSafeMap(message map[string]any) (map[string]any, error) {
	for key, val := range message {
		switch v := val.(type) {
		case *string:
			if v == nil {
				delete(message, key)
				continue
			}
			message[key] = *v
		case *uint8:
			if v == nil {
				delete(message, key)
				continue
			}
			message[key] = int(*v)
		case uint16:
			message[key] = int(v)
		case *time.Time:
			if v == nil {
				delete(message, key)
				continue
			}
			message[key] = *v
		case *map[string]map[string]string:
			if v == nil {
				delete(message, key)
				continue
			}
			message[key] = convertMap(*v)
		default:
			return nil, fmt.Errorf("key %s has unknown field of type %T", key, v)
		}
	}

	return message, nil
}

// convertMap converts map[string]map[string]string to map[string]any
// which is expected by stanza converter
func convertMap(data map[string]map[string]string) map[string]any {
	ret := map[string]any{}
	for key, value := range data {
		ret[key] = map[string]any{}
		r := ret[key].(map[string]any)

		for k, v := range value {
			r[k] = v
		}
	}

	return ret
}

func toBytes(value any) ([]byte, error) {
	switch v := value.(type) {
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("unable to convert type '%T' to bytes", value)
	}
}

var severityMapping = [...]entry.Severity{
	0: entry.Fatal,
	1: entry.Error3,
	2: entry.Error2,
	3: entry.Error,
	4: entry.Warn,
	5: entry.Info2,
	6: entry.Info,
	7: entry.Debug,
}

var severityText = [...]string{
	0: "emerg",
	1: "alert",
	2: "crit",
	3: "err",
	4: "warning",
	5: "notice",
	6: "info",
	7: "debug",
}

var severityField = entry.NewAttributeField("severity")

func cleanupTimestamp(e *entry.Entry) error {
	_, ok := entry.NewAttributeField("timestamp").Delete(e)
	if !ok {
		return fmt.Errorf("failed to cleanup timestamp")
	}

	return nil
}

func postprocessWithoutPriHeader(e *entry.Entry) error {
	return cleanupTimestamp(e)
}

func postprocess(e *entry.Entry) error {
	sev, ok := severityField.Delete(e)
	if !ok {
		return fmt.Errorf("severity field does not exist")
	}

	sevInt, ok := sev.(int)
	if !ok {
		return fmt.Errorf("severity field is not an int")
	}

	if sevInt < 0 || sevInt > 7 {
		return fmt.Errorf("invalid severity '%d'", sevInt)
	}

	e.Severity = severityMapping[sevInt]
	e.SeverityText = severityText[sevInt]

	return cleanupTimestamp(e)
}

func newOctetCountingParseFunc() parseFunc {
	return func(input []byte) (message sl.Message, err error) {
		listener := func(res *sl.Result) {
			message = res.Message
			err = res.Error
		}
		parser := octetcounting.NewParser(sl.WithBestEffort(), sl.WithListener(listener))
		reader := bytes.NewReader(input)
		parser.Parse(reader)
		return
	}
}

func newNonTransparentFramingParseFunc(trailerType nontransparent.TrailerType) parseFunc {
	return func(input []byte) (message sl.Message, err error) {
		listener := func(res *sl.Result) {
			message = res.Message
			err = res.Error
		}

		parser := nontransparent.NewParser(sl.WithBestEffort(), nontransparent.WithTrailer(trailerType), sl.WithListener(listener))
		reader := bytes.NewReader(input)
		parser.Parse(reader)
		return
	}
}
