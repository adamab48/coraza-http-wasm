package main

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"

	coreruleset "github.com/corazawaf/coraza-coreruleset"
	"github.com/corazawaf/coraza-http-wasm/operators"
	"github.com/corazawaf/coraza/v3"
	"github.com/corazawaf/coraza/v3/debuglog"
	"github.com/corazawaf/coraza/v3/types"
	httpwasm "github.com/http-wasm/http-wasm-guest-tinygo/handler"
	"github.com/http-wasm/http-wasm-guest-tinygo/handler/api"
	"github.com/jcchavezs/mergefs"
	fsio "github.com/jcchavezs/mergefs/io"
	"github.com/tidwall/gjson"
)

func init() {
	// Registers wasilibs operators before initializing the WAF.
	// See https://github.com/corazawaf/coraza-wasilibs
	operators.Register()
}

var waf coraza.WAF
var txs = map[uint32]types.Transaction{}

// main ensures buffering is available on the host.
//
// Note: required features does not include api.FeatureTrailers because some
// hosts don't support them, and the impact is minimal for logging.
func main() {
	requiredFeatures := api.FeatureBufferRequest | api.FeatureBufferResponse
	if want, have := requiredFeatures, httpwasm.Host.EnableFeatures(requiredFeatures); !have.IsEnabled(want) {
		httpwasm.Host.Log(api.LogLevelError, "Unexpected features, want: "+want.String()+", have: "+have.String())
	}
	httpwasm.HandleRequestFn = handleRequest
	httpwasm.HandleResponseFn = handleResponse

	var err error
	waf, err = initializeWAF(httpwasm.Host)
	if err != nil {
		httpwasm.Host.Log(api.LogLevelError, fmt.Sprintf("Failed to initialize WAF: %v", err))
		os.Exit(1)
	}
}

func toHostLevel(lvl debuglog.Level) api.LogLevel {
	switch lvl {
	case debuglog.LevelNoLog:
		return api.LogLevelNone
	case debuglog.LevelError:
		return api.LogLevelError
	case debuglog.LevelWarn:
		return api.LogLevelWarn
	case debuglog.LevelInfo:
		return api.LogLevelInfo
	default:
		return api.LogLevelDebug
	}
}

type config struct {
	includeCRS bool
	directives string
}

func getConfigFromHost(host api.Host) (config, error) {
	cfg := config{includeCRS: true}

	if len(host.GetConfig()) == 0 {
		return cfg, nil
	}

	var directives = strings.Builder{}
	cfgAsJSON := gjson.ParseBytes(host.GetConfig())
	if !cfgAsJSON.Exists() {
		return config{}, errors.New("invalid host config")
	}

	if includeCRSRes := cfgAsJSON.Get("includeCRS"); includeCRSRes.Exists() {
		cfg.includeCRS = includeCRSRes.Bool()
	}

	directivesResult := cfgAsJSON.Get("directives")
	if !directivesResult.IsArray() {
		return config{}, errors.New("invalid host config, array expected for field directives")
	}

	isFirst := true
	directivesResult.ForEach(func(key, value gjson.Result) bool {
		if isFirst {
			isFirst = false
		} else {
			directives.WriteByte('\n')
		}

		directives.WriteString(value.Str)
		return true
	})

	if directives.Len() == 0 {
		return config{}, errors.New("empty directives")
	}

	cfg.directives = directives.String()
	return cfg, nil
}

func errorCb(host api.Host) func(types.MatchedRule) {
	return func(mr types.MatchedRule) {
		logMsg := mr.ErrorLog()
		switch mr.Rule().Severity() {
		case types.RuleSeverityEmergency,
			types.RuleSeverityAlert,
			types.RuleSeverityCritical,
			types.RuleSeverityError:
			host.Log(api.LogLevelError, logMsg)
		case types.RuleSeverityWarning:
			host.Log(api.LogLevelWarn, logMsg)
		case types.RuleSeverityNotice,
			types.RuleSeverityInfo:
			host.Log(api.LogLevelInfo, logMsg)
		case types.RuleSeverityDebug:
			host.Log(api.LogLevelDebug, logMsg)
		}
	}
}

func initializeWAF(host api.Host) (coraza.WAF, error) {
	wafConfig := coraza.NewWAFConfig()

	if cfg, err := getConfigFromHost(host); err == nil {
		if cfg.includeCRS {
			wafConfig = wafConfig.WithRootFS(mergefs.Merge(coreruleset.FS, fsio.OSFS))
		}

		if cfg.directives == "" {
			host.Log(api.LogLevelWarn, "Initializing WAF with no directives")
		} else {
			host.Log(api.LogLevelDebug, "Initializing WAF with directives:\n"+cfg.directives)
			wafConfig = wafConfig.WithDirectives(cfg.directives)
		}
	} else {
		return nil, err
	}

	wafConfig = wafConfig.WithDebugLogger(debuglog.DefaultWithPrinterFactory(func(io.Writer) debuglog.Printer {
		return func(lvl debuglog.Level, message, fields string) {
			host.Log(toHostLevel(lvl), message+" "+fields)
			// TODO understand. 3 works.
			// But I can't print everything as error
			// host.Log(toHostLevel(3), message+" "+fields)
		}
	})).WithErrorCallback(errorCb(host))

	waf, err := coraza.NewWAF(wafConfig)
	if err != nil {
		return nil, err
	}

	return waf, nil
}

func handleRequest(req api.Request, res api.Response) (next bool, reqCtx uint32) {
	tx := waf.NewTransaction()

	// Early return, Coraza is not going to process any rule
	if tx.IsRuleEngineOff() {
		next = true
		tx.Close()
		return
	}

	defer func() {
		if tx.IsInterrupted() {
			// We run phase 5 rules and create audit logs (if enabled)
			tx.ProcessLogging()
		}

		if !next {
			// we remove temporary files and free some memory
			if err := tx.Close(); err != nil {
				tx.DebugLogger().Error().Err(err).Msg("Failed to close the transaction")
			}
		}
	}()

	var (
		client string
		cport  int
	)

	// IMPORTANT: Some http.Request.RemoteAddr implementations will not contain port or contain IPV6: [2001:db8::1]:8080
	srcAddress := req.GetSourceAddr()
	idx := strings.LastIndexByte(srcAddress, ':')
	if idx != -1 {
		client = srcAddress[:idx]
		cport, _ = strconv.Atoi(srcAddress[idx+1:])
	}

	var it *types.Interruption
	// There is no socket access in the request object, so we neither know the server client nor port.
	tx.ProcessConnection(client, cport, "", 0)
	tx.ProcessURI(req.GetURI(), req.GetMethod(), req.GetProtocolVersion())
	headers := req.Headers()
	for _, k := range headers.Names() {
		if hs := headers.GetAll(k); len(hs) > 0 {
			tx.AddRequestHeader(k, strings.Join(hs, "; "))
		}
	}

	// Host will always be removed from req.Headers() and promoted to the
	// Request.Host field, so we manually add it
	if host, ok := headers.Get("Host"); ok {
		tx.AddRequestHeader("Host", host)
		// This connector relies on the host header (now host field) to populate ServerName
		tx.SetServerName(host)
	}

	it = tx.ProcessRequestHeaders()
	if it != nil {
		handleInterruption(it, res)
		return
	}

	if tx.IsRequestBodyAccessible() {
		// We only do body buffering if the transaction requires request
		// body inspection, otherwise we just let the request follow its
		// regular flow.
		it, _, err := tx.ReadRequestBodyFrom(readWriterTo{req.Body()})
		if err != nil {
			tx.DebugLogger().Error().Err(err).Msg("Failed to read request body")
			return
		}

		if it != nil {
			handleInterruption(it, res)
			return
		}
	}

	var err error
	it, err = tx.ProcessRequestBody()
	if err != nil {
		tx.DebugLogger().Error().Err(err).Msg("Failed to process request body")
		return
	}

	if it != nil {
		handleInterruption(it, res)
		return
	}

	reqCtx = rand.Uint32()
	txs[reqCtx] = tx
	return true, reqCtx
}

func handleInterruption(in *types.Interruption, res api.Response) {
	statusCode := obtainStatusCodeFromInterruptionOrDefault(in, 403)
	res.SetStatusCode(statusCode)
}

// obtainStatusCodeFromInterruptionOrDefault returns the desired status code derived from the interruption
// on a "deny" action or a default value.
func obtainStatusCodeFromInterruptionOrDefault(it *types.Interruption, defaultStatusCode uint32) uint32 {
	if it.Action == "deny" {
		statusCode := it.Status
		if statusCode == 0 {
			statusCode = 403
		}

		return uint32(statusCode)
	}

	return defaultStatusCode
}

func handleResponse(reqCtx uint32, req api.Request, resp api.Response, isError bool) {
	if reqCtx == 0 {
		return
	}

	tx, ok := txs[reqCtx]
	if !ok {
		return
	}
	delete(txs, reqCtx)

	defer func() {
		// We run phase 5 rules and create audit logs (if enabled)
		tx.ProcessLogging()
		// we remove temporary files and free some memory
		if err := tx.Close(); err != nil {
			tx.DebugLogger().Error().Err(err).Msg("Failed to close the transaction")
		}
	}()

	if isError {
		return
	}

	// We look for interruptions triggered at phase 3 (response headers)
	// and during writing the response body. If so, response status code
	// has been sent over the flush already.
	if tx.IsInterrupted() {
		return
	}

	for _, h := range resp.Headers().Names() {
		tx.AddResponseHeader(h, strings.Join(resp.Headers().GetAll(h), ";"))
	}

	statusCode := resp.GetStatusCode()
	it := tx.ProcessResponseHeaders(int(statusCode), req.GetProtocolVersion())
	if it != nil {
		handleInterruption(it, resp)
		return
	}

	it, _, err := tx.ReadResponseBodyFrom(readWriterTo{resp.Body()})
	if err != nil {
		tx.DebugLogger().Error().Err(err).Msg("Failed to read response body")
		resp.SetStatusCode(http.StatusInternalServerError)
		return
	}
	if it != nil {
		resp.Headers().Set("Content-Length", "0")
		resp.Body().Write(nil)
		handleInterruption(it, resp)
		return
	}

	if tx.IsResponseBodyAccessible() && tx.IsResponseBodyProcessable() {
		if it, err := tx.ProcessResponseBody(); err != nil {
			resp.SetStatusCode(http.StatusInternalServerError)
			tx.DebugLogger().Error().Err(err).Msg("Failed to process response body")
			return
		} else if it != nil {
			resp.Headers().Set("Content-Length", "0")
			resp.Body().Write(nil)
			resp.SetStatusCode(obtainStatusCodeFromInterruptionOrDefault(it, statusCode))
			return
		}
	}
}
