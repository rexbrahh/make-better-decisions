package subq

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

func TestLiveSubQLongBatchedReview(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_SUBQ_LIVE") != "1" {
		t.Skip("set AGENT_DECISIONS_SUBQ_LIVE=1 for the paid Snap API check")
	}
	key, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	background := strings.Repeat("Routine background record. Preserve the configuration record.\n", 13_000)
	middle := len(background) / 2
	state := json.NewObject("observations", background[:middle]+"\nCONFIGURATION_RECORD: permitted_mode=read_only\n"+background[middle:])
	questions := json.NewObject()
	for i := range 33 {
		instruction := "Does CONFIGURATION_RECORD set permitted_mode to read_only?"
		if i%2 != 0 {
			instruction = "Does CONFIGURATION_RECORD set permitted_mode to write_enabled?"
		}
		questions.Set("q"+strconv.Itoa(i), json.NewObject("type", "noul", "instructions", instruction))
	}
	before, _ := json.Dumps(state, json.Options{})
	provider, _ := policy.Provider("subq")
	ctx, cancel := context.WithTimeout(context.Background(), provider.Timeout)
	defer cancel()
	metrics := json.NewObject()
	started := time.Now()
	answers, err := NewClient(ctx).Ask(state, questions, key, metrics)
	if err != nil {
		t.Fatal(err)
	}
	if answers.Len() != questions.Len() || metrics.Value("batch_count") != json.Int("3") {
		t.Fatal("batched review omitted requested answers")
	}
	for i, id := range questions.Keys() {
		encoded, err := json.Dumps(answers.Value(id).(*json.Object).Value("noul"), json.Options{})
		if err != nil {
			t.Fatal(err)
		}
		value, err := strconv.ParseFloat(encoded, 64)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 && value < .8 || i%2 != 0 && value > .2 {
			t.Errorf("%s: expected yes=%t, probability %.3f", id, i%2 == 0, value)
		}
	}
	after, _ := json.Dumps(state, json.Options{})
	if before != after {
		t.Fatal("review changed the supplied evidence")
	}
	usage, _ := metrics.Value("usage").(*json.Object)
	if usage == nil {
		t.Fatal("API usage was not retained")
	}
	t.Logf("model=%s batches=3 elapsed=%s input_tokens=%v", policy.SubQModel, time.Since(started).Round(time.Millisecond), usage.Value("input_tokens"))
}

func TestLiveSubQEvidenceMatrix(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_SUBQ_LIVE") != "1" {
		t.Skip("set AGENT_DECISIONS_SUBQ_LIVE=1 for the paid Snap evidence controls")
	}
	key, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, background := range []struct{ name, text string }{
		{"prose", "Build log: the routine check completed and the existing configuration was preserved.\n"},
		{"code", "func check(i int) bool { return i >= 0 } // routine build fixture\n"},
	} {
		for _, size := range []int{60_000, 780_000} {
			for placement := range 3 {
				t.Run(fmt.Sprintf("%s/%d/placement_%d", background.name, size, placement), func(t *testing.T) {
					noise := strings.Repeat(background.text, size/len(background.text)+1)[:size]
					var records strings.Builder
					questions := json.NewObject()
					truth := map[string]bool{}
					for i := range 8 {
						readOnly := (i+placement)%3 == 0
						mode := "write_enabled"
						if readOnly {
							mode = "read_only"
						}
						fmt.Fprintf(&records, "CONFIGURATION_RECORD_%02d: permitted_mode=%s\n", i, mode)
						for _, askedMode := range []string{"read_only", "write_enabled"} {
							id := fmt.Sprintf("record_%02d_%s", i, askedMode)
							truth[id] = mode == askedMode
							questions.Set(id, json.NewObject("type", "noul", "instructions",
								fmt.Sprintf("Does CONFIGURATION_RECORD_%02d set permitted_mode to %s?", i, askedMode)))
						}
					}
					wantedChoice := "write_enabled"
					if placement == 0 {
						wantedChoice = "read_only"
					}
					questions.Set("selected_mode", json.NewObject("type", "choice", "instructions",
						"Which permitted_mode does CONFIGURATION_RECORD_03 specify?", "criteria",
						json.NewObject("read_only", "read_only", "write_enabled", "write_enabled")))
					offset := placement * len(noise) / 2
					state := json.NewObject("observations", noise[:offset]+"\n"+records.String()+noise[offset:])
					encoded, err := json.Dumps(state, json.Options{})
					if err != nil {
						t.Fatal(err)
					}
					metrics := json.NewObject()
					answers, err := NewClient(context.Background()).Ask(state, questions, key, metrics)
					t.Logf("evidence_control state_sha256=%x chars=%d placement=%d metrics=%s", sha256.Sum256([]byte(encoded)), len(encoded), placement, mustDump(t, metrics))
					if err != nil {
						t.Fatal(err)
					}
					if answers.Len() != 17 || metrics.Value("batch_count") != json.Int("2") {
						t.Fatal("evidence control lost a question or batch")
					}
					for id, want := range truth {
						answer := answers.Value(id).(*json.Object)
						value, err := strconv.ParseFloat(mustDump(t, answer.Value("noul")), 64)
						if err != nil {
							t.Fatal(err)
						}
						if want && value < .8 || !want && value > .2 {
							t.Errorf("%s: want yes=%t, probability %.3f", id, want, value)
						}
					}
					if answers.Value("selected_mode").(*json.Object).Value("choice") != wantedChoice {
						t.Errorf("mode choice: want %s", wantedChoice)
					}
				})
			}
		}
	}
}

// A character budget cannot guarantee a token budget. Dense inputs must fail closed at the API limit.
func TestLiveSubQDenseContextBoundary(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_SUBQ_LIVE") != "1" {
		t.Skip("set AGENT_DECISIONS_SUBQ_LIVE=1 for the paid Snap token-boundary control")
	}
	key, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	var noise strings.Builder
	for i := range 25_000 {
		fmt.Fprintf(&noise, "%x", sha256.Sum256([]byte(strconv.Itoa(i))))
	}
	questions := json.NewObject("mode", json.NewObject("type", "choice", "instructions",
		"Which permitted_mode does CONFIGURATION_RECORD specify?", "criteria",
		json.NewObject("read_only", "read_only", "write_enabled", "write_enabled")))
	accepted, rejected := 0, 0
	for _, size := range []int{160_000, 320_000, 420_000, 480_000, 1_600_000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			state := json.NewObject("observations", noise.String()[:size/2]+"\nCONFIGURATION_RECORD: permitted_mode=read_only\n"+noise.String()[size/2:size])
			metrics := json.NewObject()
			answers, err := NewClient(context.Background()).Ask(state, questions, key, metrics)
			t.Logf("dense_context chars=%d error=%v metrics=%s", size, err, mustDump(t, metrics))
			if err != nil {
				var failure *jev.JevError
				if !errors.As(err, &failure) || failure.Reason != "http_400" || answers != nil {
					t.Fatalf("expected a sanitized context rejection without answers, got %v", err)
				}
				rejected++
				return
			}
			accepted++
			if answers.Value("mode").(*json.Object).Value("choice") != "read_only" {
				t.Fatal("accepted dense input lost its configuration record")
			}
		})
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("boundary was not observed: accepted=%d rejected=%d", accepted, rejected)
	}
}

// Five batches exercise the queue after the first four concurrent requests.
func TestLiveSubQQueuedBatchDeadline(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_SUBQ_LIVE") != "1" {
		t.Skip("set AGENT_DECISIONS_SUBQ_LIVE=1 for the paid Snap queued-batch control")
	}
	key, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	background := strings.Repeat("Routine background record. Preserve the configuration record.\n", 13_000)
	middle := len(background) / 2
	state := json.NewObject("observations", background[:middle]+"\nCONFIGURATION_RECORD: permitted_mode=read_only\n"+background[middle:])
	questions := json.NewObject()
	for i := range 65 {
		mode := "read_only"
		if i%2 != 0 {
			mode = "write_enabled"
		}
		questions.Set(fmtQuestion(i), json.NewObject("type", "noul", "instructions",
			"Does CONFIGURATION_RECORD set permitted_mode to "+mode+"?"))
	}
	metrics := json.NewObject()
	answers, err := NewClient(context.Background()).Ask(state, questions, key, metrics)
	t.Logf("queued_batches metrics=%s error=%v", mustDump(t, metrics), err)
	if err != nil {
		t.Fatal(err)
	}
	if answers.Len() != 65 || metrics.Value("batch_count") != json.Int("5") {
		t.Fatal("queued operation lost a question or batch")
	}
	for i, id := range questions.Keys() {
		value, err := strconv.ParseFloat(mustDump(t, answers.Value(id).(*json.Object).Value("noul")), 64)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 && value < .8 || i%2 != 0 && value > .2 {
			t.Errorf("%s: expected yes=%t, probability %.3f", id, i%2 == 0, value)
		}
	}
}
