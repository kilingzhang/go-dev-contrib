// Package ginzap provides log handling using zap package.
// Code structure based on ginrus package.
package ginzap

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Fn func(c *gin.Context) []zapcore.Field

// Skipper is a function to skip logs based on provided Context
type Skipper func(c *gin.Context) bool

// ZapLogger is the minimal logger interface compatible with zap.Logger
type ZapLogger interface {
	Info(msg string, fields ...zap.Field)
	Error(msg string, fields ...zap.Field)
}

// Config is config setting for Ginzap
type Config struct {
	TimeFormat      string
	UTC             bool
	SkipPaths       []string
	SkipPathRegexps []*regexp.Regexp
	Context         Fn
	DefaultLevel    zapcore.Level
	// skip is a Skipper that indicates which logs should not be written.
	// Optional.
	Skipper Skipper
	// LogRequestBody indicates whether to log the request body.
	// Optional. Default: false
	LogRequestBody bool
	// LogResponseBody indicates whether to log the response body.
	// Optional. Default: false
	LogResponseBody bool
}

// Ginzap returns a gin.HandlerFunc (middleware) that logs requests using uber-go/zap.
//
// Requests with errors are logged using zap.Error().
// Requests without errors are logged using zap.Info().
//
// It receives:
//  1. A time package format string (e.g. time.RFC3339).
//  2. A boolean stating whether to use UTC time zone or local.
func Ginzap(logger ZapLogger, timeFormat string, utc bool) gin.HandlerFunc {
	return GinzapWithConfig(logger, &Config{TimeFormat: timeFormat, UTC: utc, DefaultLevel: zapcore.InfoLevel})
}

// GinzapWithConfig returns a gin.HandlerFunc using configs
func GinzapWithConfig(logger ZapLogger, conf *Config) gin.HandlerFunc {
	skipPaths := make(map[string]bool, len(conf.SkipPaths))
	for _, path := range conf.SkipPaths {
		skipPaths[path] = true
	}

	return func(c *gin.Context) {
		start := time.Now()
		// some evil middlewares modify this values
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		// Get request body if enabled
		var requestBody string
		if conf.LogRequestBody {
			if c.Request.Body != nil {
				body, err := c.GetRawData()
				if err == nil {
					// Only log if it's valid JSON
					var js interface{}
					if json.Unmarshal(body, &js) == nil {
						requestBody = string(body)
					}
					// Restore the request body for other middleware
					c.Request.Body = io.NopCloser(strings.NewReader(string(body)))
				}
			}
		}

		// Get response body if enabled
		var blw *bodyLogWriter
		if conf.LogResponseBody {
			blw = &bodyLogWriter{body: bytes.NewBufferString(""), ResponseWriter: c.Writer}
			c.Writer = blw
		}

		c.Next()
		track := true

		if _, ok := skipPaths[path]; ok || (conf.Skipper != nil && conf.Skipper(c)) {
			track = false
		}

		if track && len(conf.SkipPathRegexps) > 0 {
			for _, reg := range conf.SkipPathRegexps {
				if !reg.MatchString(path) {
					continue
				}

				track = false
				break
			}
		}

		if track {
			end := time.Now()
			latency := end.Sub(start)
			if conf.UTC {
				end = end.UTC()
			}

			fields := []zapcore.Field{
				zap.Int("status", c.Writer.Status()),
				zap.String("method", c.Request.Method),
				zap.String("path", path),
				zap.String("query", query),
				zap.String("ip", c.ClientIP()),
				zap.String("user-agent", c.Request.UserAgent()),
				zap.Duration("latency", latency),
			}

			// Add request and response body if enabled
			if conf.LogRequestBody && requestBody != "" {
				fields = append(fields, zap.String("request-body", requestBody))
			}
			if conf.LogResponseBody && blw != nil {
				// Only log if it's valid JSON
				responseBody := blw.body.String()
				var js interface{}
				if json.Unmarshal([]byte(responseBody), &js) == nil {
					fields = append(fields, zap.String("response-body", responseBody))
				}
			}

			if conf.TimeFormat != "" {
				fields = append(fields, zap.String("time", end.Format(conf.TimeFormat)))
			}

			if conf.Context != nil {
				fields = append(fields, conf.Context(c)...)
			}

			if len(c.Errors) > 0 {
				// Append error field if this is an erroneous request.
				for _, e := range c.Errors.Errors() {
					logger.Error(e, fields...)
				}
			} else {
				if zl, ok := logger.(*zap.Logger); ok {
					zl.Log(conf.DefaultLevel, "", fields...)
				} else if conf.DefaultLevel == zapcore.InfoLevel {
					logger.Info(path, fields...)
				} else {
					logger.Error(path, fields...)
				}
			}
		}
	}
}

// bodyLogWriter is a custom ResponseWriter that captures the response body
type bodyLogWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w *bodyLogWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func defaultHandleRecovery(c *gin.Context, err interface{}) {
	c.AbortWithStatus(http.StatusInternalServerError)
}

// RecoveryWithZap returns a gin.HandlerFunc (middleware)
// that recovers from any panics and logs requests using uber-go/zap.
// All errors are logged using zap.Error().
// stack means whether output the stack info.
// The stack info is easy to find where the error occurs but the stack info is too large.
func RecoveryWithZap(logger ZapLogger, stack bool) gin.HandlerFunc {
	return CustomRecoveryWithZap(logger, stack, defaultHandleRecovery)
}

// CustomRecoveryWithZap returns a gin.HandlerFunc (middleware) with a custom recovery handler
// that recovers from any panics and logs requests using uber-go/zap.
// All errors are logged using zap.Error().
// stack means whether output the stack info.
// The stack info is easy to find where the error occurs but the stack info is too large.
func CustomRecoveryWithZap(logger ZapLogger, stack bool, recovery gin.RecoveryFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Check for a broken connection, as it is not really a
				// condition that warrants a panic stack trace.
				var brokenPipe bool
				if ne, ok := err.(*net.OpError); ok {
					if se, ok := ne.Err.(*os.SyscallError); ok {
						if strings.Contains(strings.ToLower(se.Error()), "broken pipe") ||
							strings.Contains(strings.ToLower(se.Error()), "connection reset by peer") {
							brokenPipe = true
						}
					}
				}

				httpRequest, _ := httputil.DumpRequest(c.Request, false)
				if brokenPipe {
					logger.Error(c.Request.URL.Path,
						zap.Any("error", err),
						zap.String("request", string(httpRequest)),
					)
					// If the connection is dead, we can't write a status to it.
					c.Error(err.(error)) //nolint: errcheck
					c.Abort()
					return
				}

				if stack {
					logger.Error("[Recovery from panic]",
						zap.Time("time", time.Now()),
						zap.Any("error", err),
						zap.String("request", string(httpRequest)),
						zap.String("stack", string(debug.Stack())),
					)
				} else {
					logger.Error("[Recovery from panic]",
						zap.Time("time", time.Now()),
						zap.Any("error", err),
						zap.String("request", string(httpRequest)),
					)
				}
				recovery(c, err)
			}
		}()
		c.Next()
	}
}
