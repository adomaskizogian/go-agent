package internal

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/newrelic/go-sdk/api"
	"github.com/newrelic/go-sdk/log"
)

type txnInput struct {
	Writer     http.ResponseWriter
	Request    *http.Request
	Config     api.Config
	Reply      *ConnectReply
	Consumer   dataConsumer
	attrConfig *attributeConfig
}

type txn struct {
	txnInput
	// This mutex is required since the consumer may call the public API
	// interface functions from different routines.
	sync.Mutex
	// finished indicates whether or not End() has been called.  After
	// finished has been set to true, no recording should occur.
	finished   bool
	queuing    time.Duration
	start      time.Time
	name       string // Work in progress name
	isWeb      bool
	ignore     bool
	errors     txnErrors // Lazily initialized.
	errorsSeen uint64
	attrs      *attributes

	// wroteHeader prevents capturing multiple response code errors if the
	// user erroneously calls WriteHeader multiple times.
	wroteHeader bool

	// Fields assigned at completion
	stop           time.Time
	duration       time.Duration
	finalName      string // Full finalized metric name
	zone           apdexZone
	apdexThreshold time.Duration
}

func newTxn(input txnInput, name string) *txn {
	txn := &txn{
		txnInput: input,
		start:    time.Now(),
		name:     name,
		isWeb:    nil != input.Request,
		attrs:    newAttributes(input.attrConfig),
	}
	if nil != txn.Request {
		h := input.Request.Header
		txn.attrs.agent.RequestMethod = input.Request.Method
		txn.attrs.agent.RequestAcceptHeader = h.Get("Accept")
		txn.attrs.agent.RequestContentType = h.Get("Content-Type")
		txn.attrs.agent.RequestHeadersHost = h.Get("Host")
		txn.attrs.agent.RequestHeadersUserAgent = h.Get("User-Agent")
		txn.attrs.agent.RequestHeadersReferer = safeURLFromString(h.Get("Referer"))

		if cl := h.Get("Content-Length"); "" != cl {
			if x, err := strconv.Atoi(cl); nil == err {
				txn.attrs.agent.RequestContentLength = x
			}
		}

		txn.queuing = queueDuration(h, txn.start)
	}

	txn.attrs.agent.HostDisplayName = txn.Config.HostDisplayName

	return txn
}

func (txn *txn) txnEventsEnabled() bool {
	return txn.Config.TransactionEvents.Enabled &&
		txn.Reply.CollectAnalyticsEvents
}

func (txn *txn) errorEventsEnabled() bool {
	return txn.Config.ErrorCollector.CaptureEvents &&
		txn.Reply.CollectErrorEvents
}

func (txn *txn) freezeName() {
	if txn.ignore || ("" != txn.finalName) {
		return
	}

	txn.finalName = CreateFullTxnName(txn.name, txn.Reply, txn.isWeb)
	if "" == txn.finalName {
		txn.ignore = true
	}
}

func (txn *txn) getsApdex() bool {
	return txn.isWeb
}

func (txn *txn) mergeIntoHarvest(h *harvest) {
	h.createTxnMetrics(createTxnMetricsArgs{
		IsWeb:          txn.isWeb,
		Duration:       txn.duration,
		Name:           txn.finalName,
		Zone:           txn.zone,
		ApdexThreshold: txn.apdexThreshold,
		ErrorsSeen:     txn.errorsSeen,
	})

	if txn.queuing > 0 {
		h.metrics.addDuration(queueMetric, "", txn.queuing, txn.queuing, forced)
	}

	if txn.txnEventsEnabled() {
		event := &txnEvent{
			Name:      txn.finalName,
			Timestamp: txn.start,
			Duration:  txn.duration,
			queuing:   txn.queuing,
			zone:      txn.zone,
			attrs:     txn.attrs,
		}
		h.addTxnEvent(event)
	}

	requestURI := ""
	if nil != txn.Request && nil != txn.Request.URL {
		requestURI = safeURL(txn.Request.URL)
	}

	mergeTxnErrors(h.errorTraces, txn.errors, txn.finalName, requestURI, txn.attrs)

	if txn.errorEventsEnabled() {
		for _, e := range txn.errors {
			h.errorEvents.Add(&errorEvent{
				klass:    e.klass,
				msg:      e.msg,
				when:     e.when,
				txnName:  txn.finalName,
				duration: txn.duration,
				queuing:  txn.queuing,
				attrs:    txn.attrs,
			})
		}
	}
}

func responseCodeIsError(cfg *api.Config, code int) bool {
	if code < http.StatusBadRequest { // 400
		return false
	}
	for _, ignoreCode := range cfg.ErrorCollector.IgnoreStatusCodes {
		if code == ignoreCode {
			return false
		}
	}
	return true
}

var (
	// statusCodeLookup avoids a strconv.Itoa call.
	statusCodeLookup = map[int]string{
		100: "100", 101: "101",
		200: "200", 201: "201", 202: "202", 203: "203", 204: "204", 205: "205", 206: "206",
		300: "300", 301: "301", 302: "302", 303: "303", 304: "304", 305: "305", 307: "307",
		400: "400", 401: "401", 402: "402", 403: "403", 404: "404", 405: "405", 406: "406",
		407: "407", 408: "408", 409: "409", 410: "410", 411: "411", 412: "412", 413: "413",
		414: "414", 415: "415", 416: "416", 417: "417", 418: "418", 428: "428", 429: "429",
		431: "431", 451: "451",
		500: "500", 501: "501", 502: "502", 503: "503", 504: "504", 505: "505", 511: "511",
	}
)

func headersJustWritten(txn *txn, code int) {
	if txn.finished {
		return
	}
	if txn.wroteHeader {
		return
	}
	txn.wroteHeader = true

	h := txn.Writer.Header()

	txn.attrs.agent.ResponseHeadersContentType = h.Get("Content-Type")

	if val := h.Get("Content-Length"); "" != val {
		if x, err := strconv.Atoi(val); nil == err {
			txn.attrs.agent.ResponseHeadersContentLength = x
		}
	}

	txn.attrs.agent.ResponseCode = statusCodeLookup[code]
	if txn.attrs.agent.ResponseCode == "" {
		txn.attrs.agent.ResponseCode = strconv.Itoa(code)
	}

	if responseCodeIsError(&txn.Config, code) {
		e := txnErrorFromResponseCode(code)
		e.stack = getStackTrace(1)
		txn.noticeErrorInternal(e)
	}
}

func (txn *txn) Header() http.Header { return txn.Writer.Header() }

func (txn *txn) Write(b []byte) (int, error) {
	n, err := txn.Writer.Write(b)

	txn.Lock()
	defer txn.Unlock()

	headersJustWritten(txn, http.StatusOK)

	return n, err
}

func (txn *txn) WriteHeader(code int) {
	txn.Writer.WriteHeader(code)

	txn.Lock()
	defer txn.Unlock()

	headersJustWritten(txn, code)
}

var (
	// ErrAlreadyEnded is returned by public txn methods if End() has
	// already been called.
	ErrAlreadyEnded = errors.New("transaction has already ended")
)

func (txn *txn) End() error {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return ErrAlreadyEnded
	}

	txn.finished = true

	r := recover()
	if nil != r {
		e := txnErrorFromPanic(r)
		e.stack = getStackTrace(0)
		txn.noticeErrorInternal(e)
	}

	txn.stop = time.Now()
	txn.duration = txn.stop.Sub(txn.start)

	txn.freezeName()
	if txn.getsApdex() {
		txn.apdexThreshold = calculateApdexThreshold(txn.Reply, txn.finalName)
		if txn.errorsSeen > 0 {
			txn.zone = apdexFailing
		} else {
			txn.zone = calculateApdexZone(txn.apdexThreshold, txn.duration)
		}
	} else {
		txn.zone = apdexNone
	}

	// This logging adds roughly 4 allocations per transaction.
	log.Debug("transaction ended", log.Context{
		"name":        txn.finalName,
		"duration_ms": txn.duration.Seconds() * 1000.0,
	})

	if !txn.ignore {
		txn.Consumer.consume(txn.Reply.RunID, txn)
	}

	// Note that if a consumer uses `panic(nil)`, the panic will not
	// propogate.
	if nil != r {
		panic(r)
	}

	return nil
}

func (txn *txn) AddAttribute(name string, value interface{}) error {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return ErrAlreadyEnded
	}

	return addUserAttribute(txn.attrs, name, value, destAll)
}

var (
	// ErrorsLocallyDisabled is returned if error capture is disabled by
	// local configuration.
	ErrorsLocallyDisabled = errors.New("errors locally disabled")
	// ErrorsRemotelyDisabled is returned if error capture is disabled
	// by remote configuration.
	ErrorsRemotelyDisabled = errors.New("errors remotely disabled")
	// ErrNilError is returned if the provided error is nil.
	ErrNilError = errors.New("nil error")
)

const (
	// HighSecurityErrorMsg is used in place of the error's message
	// (err.String()) when high security moed is enabled.
	HighSecurityErrorMsg = "message removed by high security setting"
)

func (txn *txn) noticeErrorInternal(err txnError) error {
	// Increment errorsSeen even if errors are disabled:  Error metrics do
	// not depend on whether or not errors are enabled.
	txn.errorsSeen++

	if !txn.Config.ErrorCollector.Enabled {
		return ErrorsLocallyDisabled
	}

	if !txn.Reply.CollectErrors {
		return ErrorsRemotelyDisabled
	}

	if nil == txn.errors {
		txn.errors = newTxnErrors(maxTxnErrors)
	}

	if txn.Config.HighSecurity {
		err.msg = HighSecurityErrorMsg
	}

	err.when = time.Now()

	txn.errors.Add(&err)

	return nil
}

func (txn *txn) NoticeError(err error) error {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return ErrAlreadyEnded
	}

	if nil == err {
		return ErrNilError
	}

	e := txnErrorFromError(err)
	e.stack = getStackTrace(1)
	return txn.noticeErrorInternal(e)
}

func (txn *txn) SetName(name string) error {
	txn.Lock()
	defer txn.Unlock()

	if txn.finished {
		return ErrAlreadyEnded
	}

	txn.name = name
	return nil
}
