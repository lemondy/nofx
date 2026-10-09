package stockengine

import (
	"errors"
	"testing"
	"time"

	"nofx/mcp"
)

func TestParseDecisions(t *testing.T) {
	tests := []struct {
		name, response        string
		symbol, action, entry string
		count                 int
		fail                  bool
	}{
		{"fenced", "Summary\n```json\n[{\"symbol\":\"aaplbusdt\",\"action\":\"OPEN_LONG\",\"entry_type\":\"LIMIT\"}]\n```\nTrailing prose", "AAPLBUSDT", ActionOpenLong, EntryLimit, 1, false},
		{"raw trailing", `Thoughts [{"symbol":" MSFTbusdt ","action":" wait "}] done`, "MSFTBUSDT", ActionWait, "", 1, false},
		{"unknown dropped", `[{"symbol":"A","action":"open_short"},{"symbol":"aaplbusdt","action":"hold"}]`, "AAPLBUSDT", ActionHold, "", 1, false},
		{"last array", `[{"symbol":"OLD","action":"wait"}] prose [{"symbol":"aaplbusdt","action":"open_long","reasoning":"brackets [inside] and escaped quote \""}] trailing`, "AAPLBUSDT", ActionOpenLong, "", 1, false},
		{"last valid array", `[{"symbol":"aaplbusdt","action":"wait"}] junk [not valid JSON]`, "AAPLBUSDT", ActionWait, "", 1, false},
		{"empty", `Summary [] trailing`, "", "", "", 0, false},
		{"all unknown", `[{"symbol":"A","action":"sell_short"}]`, "", "", "", 0, false},
		{"absent", `No decision array`, "", "", "", 0, true},
		{"malformed", `[{"symbol":"A"`, "", "", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseDecisions(tt.response)
			if tt.fail {
				if err == nil {
					t.Fatal("expected parse error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(d) != tt.count {
				t.Fatal(d)
			}
			if len(d) > 0 && (d[0].Symbol != tt.symbol || d[0].Action != tt.action || d[0].EntryType != tt.entry) {
				t.Fatal(d)
			}
		})
	}
}

type fakeClient struct {
	response     string
	err          error
	system, user string
}

var _ mcp.AIClient = (*fakeClient)(nil)

func (*fakeClient) SetAPIKey(string, string, string) {}
func (*fakeClient) SetTimeout(time.Duration)         {}
func (f *fakeClient) CallWithMessages(system, user string) (string, error) {
	f.system, f.user = system, user
	return f.response, f.err
}
func (*fakeClient) CallWithRequest(*mcp.Request) (string, error) { return "", errors.New("unused") }
func (*fakeClient) CallWithRequestStream(*mcp.Request, func(string)) (string, error) {
	return "", errors.New("unused")
}
func (*fakeClient) CallWithRequestFull(*mcp.Request) (*mcp.LLMResponse, error) {
	return nil, errors.New("unused")
}

func TestDecideFakeClient(t *testing.T) {
	c := testContext()
	c.Now = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeClient{response: `Summary [{"symbol":"aaplbusdt","action":"open_long","stop_loss":96},{"symbol":"MSFTBUSDT","action":"open_long","stop_loss":100},{"symbol":"QQQBUSDT","action":"wait"}]`}
	r, err := Decide(c, f, "en")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Decisions) != 3 || len(r.Verdicts) != 3 {
		t.Fatal(r)
	}
	assertCode(t, r.Verdicts[0], "")
	assertCode(t, r.Verdicts[1], "STOP_INVALID")
	assertCode(t, r.Verdicts[2], "")
	if r.SystemPrompt == "" || r.UserPrompt == "" || r.SystemPrompt != f.system || r.UserPrompt != f.user || r.RawResponse != f.response {
		t.Fatal("result/call mismatch")
	}
	if r.DurationMs < 0 || r.DurationMs > 1000 {
		t.Fatalf("duration used trading clock: %d", r.DurationMs)
	}
}

func TestDecideErrorsPreserveResponse(t *testing.T) {
	for _, callErr := range []error{nil, errors.New("fake failure")} {
		f := &fakeClient{response: "unparseable raw response", err: callErr}
		r, err := Decide(testContext(), f, "zh")
		if err == nil || r == nil || r.RawResponse != f.response || r.SystemPrompt == "" || len(r.Verdicts) != 0 {
			t.Fatalf("result=%+v error=%v", r, err)
		}
	}
}
