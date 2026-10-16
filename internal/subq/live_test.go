package subq

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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
