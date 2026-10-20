// Package subq implements the Snap transport used for reviewed decisions.
package subq

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	pyjson "github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
)

const (
	maxQuestionsPerRequest = 16
	maxConcurrentRequests  = 4
	maxRequestBytes        = 2 << 20
	maxResponseBytes       = 1 << 20
)

var keyFile = policy.SubQKeyFile

// Client shares a request limit across all Ask calls that use it.
type Client struct {
	ctx     context.Context
	http    *http.Client
	slots   chan struct{}
	timeout time.Duration
}

// NewClient uses the fixed policy endpoint and Go's standard verified HTTPS transport.
func NewClient(ctx context.Context) *Client {
	if ctx == nil {
		ctx = context.Background()
	}
	transport := http.DefaultTransport
	if standard, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = standard.Clone()
	}
	provider, _ := policy.Provider("subq")
	return &Client{
		ctx:     ctx,
		http:    &http.Client{Transport: transport, CheckRedirect: rejectRedirect},
		slots:   make(chan struct{}, maxConcurrentRequests),
		timeout: provider.Timeout,
	}
}

// LoadKey returns the API key from SUBQ_API_KEY, else from the managed SOPS-Nix key file.
func LoadKey() (string, error) {
	key := pystr.Strip(os.Getenv("SUBQ_API_KEY"))
	if key == "" {
		path, err := keyFile()
		if err != nil {
			return "", err
		}
		key, err = pathlib.ReadText(path)
		if err != nil {
			if !errors.As(err, new(*pyerr.OSError)) {
				return "", err
			}
			key = ""
		}
		key = pystr.Strip(key)
	}
	if key == "" {
		return "", &jev.JevError{Reason: "no_key"}
	}
	return key, nil
}

// Ask sends deterministic Snap batches and returns answers only when every batch validates.
func (c *Client) Ask(state, questions *pyjson.Object, key string, metrics *pyjson.Object) (*pyjson.Object, error) {
	if c == nil || c.http == nil || c.slots == nil || c.timeout <= 0 {
		return nil, &jev.JevError{Reason: "unreachable"}
	}
	if state == nil || questions == nil || questions.Len() == 0 {
		return nil, &jev.JevError{Reason: "malformed", Detail: "request JSON"}
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.timeout)
	defer cancel()
	started := time.Now()
	batches := splitQuestions(questions)
	results := make([]batchResult, len(batches))
	workerCount := min(maxConcurrentRequests, len(batches))
	var workers sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	jobs := make(chan int, workerCount)
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				result := c.requestBatch(ctx, state, batches[index], key)
				results[index] = result
				if result.err != nil {
					firstErrOnce.Do(func() {
						firstErr = result.err
						cancel()
					})
				}
			}
		}()
	}
	for index := range batches {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()

	if metrics != nil {
		if err := mergeMetrics(metrics, results, len(batches), time.Since(started)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, contextFailure(err)
	}
	answers := &pyjson.Object{}
	for index, batch := range batches {
		if results[index].answers == nil {
			return nil, &jev.JevError{Reason: "malformed", Detail: "response JSON or answers object"}
		}
		for _, id := range batch.Keys() {
			answers.Set(id, results[index].answers.Value(id))
		}
	}
	return answers, nil
}

type batchResult struct {
	answers      *pyjson.Object
	usage        *pyjson.Object
	requestBytes int
	model        string
	err          error
}

func splitQuestions(questions *pyjson.Object) []*pyjson.Object {
	var batches []*pyjson.Object
	var batch *pyjson.Object
	for _, id := range questions.Keys() {
		if batch == nil {
			batch = &pyjson.Object{}
		}
		batch.Set(id, questions.Value(id))
		if batch.Len() == maxQuestionsPerRequest {
			batches = append(batches, batch)
			batch = nil
		}
	}
	if batch != nil {
		batches = append(batches, batch)
	}
	return batches
}

func (c *Client) requestBatch(ctx context.Context, state, questions *pyjson.Object, key string) batchResult {
	var result batchResult
	if err := ctx.Err(); err != nil {
		result.err = contextFailure(err)
		return result
	}
	text, err := pyjson.Dumps(pyjson.NewObject("model", policy.SubQModel, "state", state, "questions", questions),
		pyjson.Options{})
	if err != nil {
		result.err = &jev.JevError{Reason: "malformed", Detail: "request JSON"}
		return result
	}
	body := []byte(text)
	result.requestBytes = len(body)
	if len(body) > maxRequestBytes {
		result.err = &jev.JevError{Reason: "request_too_large"}
		return result
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		result.err = contextFailure(ctx.Err())
		return result
	}
	defer func() { <-c.slots }()
	if err := ctx.Err(); err != nil {
		result.err = contextFailure(err)
		return result
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, policy.SubQURL, bytes.NewReader(body))
	if err != nil {
		result.err = &jev.JevError{Reason: "unreachable"}
		return result
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Close = true
	response, err := c.http.Do(request)
	if err != nil {
		result.err = transportFailure(err, "request")
		return result
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.err = &jev.JevError{Reason: "http_" + strconv.Itoa(response.StatusCode)}
		return result
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		result.err = transportFailure(err, "response_body")
		return result
	}
	if len(raw) > maxResponseBytes {
		result.err = &jev.JevError{Reason: "response_too_large"}
		return result
	}
	batchMetrics := &pyjson.Object{}
	answers, err := jev.ValidateResponse(raw, questions, batchMetrics)
	if err != nil {
		var jevError *jev.JevError
		if errors.As(err, &jevError) {
			result.err = jevError
		} else {
			result.err = &jev.JevError{Reason: "malformed", Detail: "response JSON or answers object"}
		}
		return result
	}
	model, ok := batchMetrics.Value("model").(string)
	if !ok || model != policy.SubQModel {
		result.err = &jev.JevError{Reason: "malformed", Detail: "response model"}
		return result
	}
	result.answers = answers
	result.model = model
	result.usage, _ = batchMetrics.Value("usage").(*pyjson.Object)
	return result
}

func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func contextFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return &jev.JevError{Reason: "cancelled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &jev.JevError{Reason: "timeout"}
	}
	return &jev.JevError{Reason: "unreachable"}
}

func transportFailure(err error, phase string) error {
	reason := "unreachable"
	if errors.Is(err, context.Canceled) {
		reason = "cancelled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		reason = "timeout"
	} else {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			reason = "timeout"
		}
	}
	return &jev.JevError{Reason: reason, Phase: phase}
}

func mergeMetrics(metrics *pyjson.Object, results []batchResult, batchCount int, elapsed time.Duration) error {
	requestSizes := make([]any, 0, batchCount)
	usage, err := sumUsage(results)
	if err != nil {
		return err
	}
	allModelsValid := batchCount > 0
	for _, result := range results {
		if result.requestBytes > 0 {
			requestSizes = append(requestSizes, pyjson.IntOf(int64(result.requestBytes)))
		}
		if result.err != nil || result.model != policy.SubQModel {
			allModelsValid = false
		}
	}
	metrics.Set("provider", "subq")
	metrics.Set("requested_model", policy.SubQModel)
	metrics.Delete("returned_model")
	if allModelsValid {
		metrics.Set("returned_model", policy.SubQModel)
	}
	metrics.Set("batch_count", pyjson.IntOf(int64(batchCount)))
	metrics.Set("request_sizes", requestSizes)
	metrics.Set("api_ms", float64(elapsed.Nanoseconds())/1e6)
	metrics.Set("usage", usage)
	return nil
}

type usageTotal struct {
	integerOnly bool
	integer     big.Int
	floating    float64
}

func sumUsage(results []batchResult) (*pyjson.Object, error) {
	totals := make(map[string]*usageTotal)
	order := []string{}
	for _, result := range results {
		if result.usage == nil {
			continue
		}
		for _, name := range result.usage.Keys() {
			value := result.usage.Value(name)
			total, ok := totals[name]
			if !ok {
				total = &usageTotal{integerOnly: true}
				totals[name] = total
				order = append(order, name)
			}
			if err := total.add(value); err != nil {
				return nil, &jev.JevError{Reason: "malformed", Detail: "usage"}
			}
		}
	}
	usage := &pyjson.Object{}
	for _, name := range order {
		total := totals[name]
		if total.integerOnly {
			usage.Set(name, pyjson.Int(total.integer.String()))
		} else {
			usage.Set(name, total.floating)
		}
	}
	return usage, nil
}

func (total *usageTotal) add(value any) error {
	switch value := value.(type) {
	case pyjson.Int:
		var integer big.Int
		if _, ok := integer.SetString(string(value), 10); !ok {
			return errors.New("invalid integer usage")
		}
		if total.integerOnly {
			total.integer.Add(&total.integer, &integer)
			converted, err := strconv.ParseFloat(total.integer.String(), 64)
			if err != nil || math.IsInf(converted, 0) || math.IsNaN(converted) {
				return errors.New("non-finite integer usage")
			}
			return nil
		}
		converted, err := strconv.ParseFloat(string(value), 64)
		if err != nil || math.IsInf(converted, 0) || math.IsNaN(converted) {
			return errors.New("non-finite integer usage")
		}
		total.floating += converted
	case float64:
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return errors.New("non-finite usage")
		}
		if total.integerOnly {
			converted, err := strconv.ParseFloat(total.integer.String(), 64)
			if err != nil || math.IsInf(converted, 0) || math.IsNaN(converted) {
				return errors.New("non-finite integer usage")
			}
			total.floating = converted
			total.integerOnly = false
		}
		total.floating += value
	default:
		return nil
	}
	if !total.integerOnly && (math.IsInf(total.floating, 0) || math.IsNaN(total.floating)) {
		return errors.New("non-finite usage sum")
	}
	return nil
}
