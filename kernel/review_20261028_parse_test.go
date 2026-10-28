package kernel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"nofx/logger"
)

func TestReview20261028TolerantDecisionFields(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		want   Decision
	}{
		{"decimal int", `"leverage":3.0`, Decision{Leverage: 3}},
		{"string int", `"leverage":"3"`, Decision{Leverage: 3}},
		{"fractional int", `"leverage":3.4`, Decision{Leverage: 3}},
		{"negative half", `"leverage":-3.5`, Decision{Leverage: -4}},
		{"confidence half", `"confidence":85.5`, Decision{Confidence: 86}},
		{"string confidence half", `"confidence":"85.5"`, Decision{Confidence: 86}},
		// take_profit_pct is an unknown field on Decision, just as before.
		// Also exercise the leading plus on a float field that is consumed.
		{"bare plus", `"take_profit_pct":+1.1,"take_profit": +1.1`, Decision{TakeProfit: 1.1}},
		{"string float", `"take_profit":"1.1"`, Decision{TakeProfit: 1.1}},
		{"string plus", `"take_profit":"+1.1"`, Decision{TakeProfit: 1.1}},
		{"scientific int", `"leverage":3e0`, Decision{Leverage: 3}},
		{"escaped numeric string", `"leverage":"\u0033.0"`, Decision{Leverage: 3}},
		{"curly quotes in reasoning", `"reasoning":"突破“关键位”失败"`, Decision{Reasoning: "突破“关键位”失败"}},
	}
	before := Decision{Symbol: "BTCUSDT", Action: "close_long", Reasoning: "exit"}
	after := Decision{Symbol: "SOLUSDT", Action: "adjust_stop_loss", StopLoss: 150, Reasoning: "protect"}
	for _, tc := range cases {
		for _, fenced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fenced=%t", tc.name, fenced), func(t *testing.T) {
				body := `[{"symbol":"BTCUSDT","action":"close_long","reasoning":"exit"},` +
					`{"symbol":"ETHUSDT","action":"hold",` + tc.fields + `},` +
					`{"symbol":"SOLUSDT","action":"adjust_stop_loss","stop_loss":150,"reasoning":"protect"}]`
				if fenced {
					body = "```json\n" + body + "\n```"
				}
				got, err := extractDecisions("<decision>" + body + "</decision>")
				wantTarget := tc.want
				wantTarget.Symbol, wantTarget.Action = "ETHUSDT", "hold"
				want := []Decision{before, wantTarget, after}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("got %+v, err=%v; want %+v", got, err, want)
				}
			})
		}
	}
}

func TestReview20261028MissingNumericValues(t *testing.T) {
	for _, value := range []string{`NaN`, `Infinity`, `+Infinity`, `-Infinity`, `null`, `""`, `"NaN"`, `"Infinity"`, `"+Infinity"`, `"-Infinity"`} {
		t.Run(value, func(t *testing.T) {
			body := fmt.Sprintf(`[{"symbol":"ETHUSDT","action":"hold","leverage":%s,"risk_usd":%s,"entry_quality":%s,"management_quality":%s},{"symbol":"BTCUSDT","action":"close_long"}]`, value, value, value, value)
			got, err := extractDecisions(body)
			want := []Decision{{Symbol: "ETHUSDT", Action: "hold"}, {Symbol: "BTCUSDT", Action: "close_long"}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, err=%v; want %+v", got, err, want)
			}
		})
	}
}

func TestReview20261028AllNumericFields(t *testing.T) {
	response := `[{"symbol":"ETHUSDT","action":"place_buy_limit",
		"leverage":"3", "position_size_usd":"250.5", "stop_loss":"1900.1",
		"take_profit":"2200.2", "price":"2000.3", "quantity":"0.125",
		"level_index":"2.5", "confidence":"85.5", "risk_usd":"12.5",
		"close_fraction":"0.25", "entry_quality":"80.5",
		"management_quality":"72.4", "tp_option":"1.5"}]`
	entry, management := 81, 72
	want := []Decision{{
		Symbol: "ETHUSDT", Action: "place_buy_limit", Leverage: 3,
		PositionSizeUSD: 250.5, StopLoss: 1900.1, TakeProfit: 2200.2,
		Price: 2000.3, Quantity: 0.125, LevelIndex: 3, Confidence: 86,
		RiskUSD: 12.5, CloseFraction: 0.25, EntryQuality: &entry,
		ManagementQuality: &management, TPOption: 2,
	}}
	got, err := extractDecisions(response)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, err=%v; want %+v", got, err, want)
	}
}

func TestReview20261028DropOnlyUndecodableObject(t *testing.T) {
	for _, bad := range []string{
		`"leverage":{"x":1}`,
		`"risk_usd":[1,2]`,
		`"leverage":true`,
		`"leverage":"not a number"`,
		`"confidence":1e100`,
		`"reasoning":{"x":1}`,
	} {
		t.Run(bad, func(t *testing.T) {
			response := `[{"symbol":"BTCUSDT","action":"close_long"},` +
				`{"symbol":"BADUSDT","action":"hold",` + bad + `},` +
				`{"symbol":"SOLUSDT","action":"partial_close_long","close_fraction":0.25}]`
			got, err := extractDecisions(response)
			want := []Decision{{Symbol: "BTCUSDT", Action: "close_long"}, {Symbol: "SOLUSDT", Action: "partial_close_long", CloseFraction: 0.25}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, err=%v; want %+v", got, err, want)
			}
		})
	}
	response := `[{"symbol":"A","leverage":{"x":1}},{"symbol":"B","confidence":[1]}]`
	if got, err := extractDecisions(response); err == nil {
		t.Fatalf("all objects bad must fail, got %+v", got)
	}
}

func TestReview20261028LastDecisionPayload(t *testing.T) {
	draft := `[{"symbol":"ETHUSDT","action":"close_long"}]`
	final := `[{"symbol":"ETHUSDT","action":"hold"}]`
	cases := map[string]string{
		"draft inside reasoning": `<reasoning>draft: <decision>` + draft + `</decision> hmm no...</reasoning><decision>` + final + `</decision>`,
		"three complete blocks":  `<decision>` + draft + `</decision><decision>` + draft + `</decision><decision>` + final + `</decision>`,
		"nested open mention":    `<reasoning>draft: <decision>` + draft + ` hmm no...</reasoning><decision>` + final + `</decision>`,
		"untagged arrays":        "draft: " + draft + "\nfinal: " + final,
		"untagged fences":        "```json\n" + draft + "\n```\nfinal:\n```json\n" + final + "\n```",
		"fence then bare":        "```json\n" + draft + "\n```\nfinal: " + final,
		"bare then fence":        "draft: " + draft + "\nfinal:\n```json\n" + final + "\n```",
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := extractDecisions(response)
			want := []Decision{{Symbol: "ETHUSDT", Action: "hold"}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, err=%v; want %+v", got, err, want)
			}
		})
	}
	for _, response := range []string{
		`<decision>` + draft + `</decision><decision>[]</decision>`,
		"draft: " + draft + "\n```json [] ```",
		"```json\n" + draft + "\n```\n[]",
	} {
		if got, err := extractDecisions(response); err != nil || len(got) != 0 {
			t.Fatalf("last empty array must remain valid, got %+v err=%v", got, err)
		}
	}
	badFinal := `<decision>` + draft + `</decision><decision>[{"leverage":{"x":1}}]</decision>`
	if got, err := extractDecisions(badFinal); err == nil {
		t.Fatalf("undecodable final must not fall back to draft, got %+v", got)
	}
}

func TestReview20261028UnclosedFinalBoundary(t *testing.T) {
	prefix := `<reasoning>draft: <decision>[{"symbol":"ETHUSDT","action":"close_long"}]</decision></reasoning><decision>`
	got, err := extractDecisions(prefix + `[{"symbol":"ETHUSDT","action":"hold"},{"symbol":"SOL`)
	want := []Decision{{Symbol: "ETHUSDT", Action: "hold"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("only final payload may be salvaged, got %+v err=%v", got, err)
	}
	for _, payload := range []string{"", `[{"symbol":"SOL`, "[]"} {
		if got, err := extractDecisions(prefix + payload); err == nil {
			t.Fatalf("unclosed final without a complete object must fail, got %+v", got)
		}
	}
}

func TestReview20261028PunctuationOutsideStrings(t *testing.T) {
	reason := `突破“关键位”失败，价格：未确认；［］｛｝【】〔〕、　‘保留’；"escaped" \ :+1.1 NaN Infinity`
	encoded, err := json.Marshal(reason)
	if err != nil {
		t.Fatal(err)
	}
	response := `［｛"symbol"："ETHUSDT"，"action"："hold"、"reasoning"：` + string(encoded) + `｝］`
	got, err := extractDecisions(response)
	if err != nil || len(got) != 1 || got[0].Reasoning != reason {
		t.Fatalf("reasoning must be preserved exactly, got %+v err=%v; want %q", got, err, reason)
	}
	for _, raw := range []string{
		string(encoded),
		`{"reasoning":` + string(encoded) + `,"risk_usd":1.1}`,
		// Even invalid UTF-8 bytes inside a string must not be rewritten.
		"\"" + string([]byte{0xff}) + "\"",
	} {
		if fixed := fixMissingQuotes(raw); fixed != raw {
			t.Fatalf("ASCII-quoted contents changed: got %q want %q", fixed, raw)
		}
		if normalized := normalizeDecisionNumbers(raw); normalized != raw {
			t.Fatalf("numeric cleanup changed quoted text: got %q want %q", normalized, raw)
		}
	}
	for _, raw := range []string{
		`【｛“symbol”：“ETHUSDT”，“action”：“hold”｝】`,
		`〔｛‘symbol’：‘ETHUSDT’、‘action’：‘hold’｝〕`,
		`［　｛"symbol"："ETHUSDT"，"action"："hold"｝　］`,
	} {
		got, err := extractDecisions(raw)
		want := []Decision{{Symbol: "ETHUSDT", Action: "hold"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("structural punctuation not repaired: got %+v err=%v", got, err)
		}
	}
}

func TestReview20261028SafeFallbackUnchanged(t *testing.T) {
	for _, response := range []string{"", "analysis: no actionable setup", "[1,2,3]"} {
		got, err := extractDecisions(response)
		if err != nil || len(got) != 1 || got[0].Symbol != "ALL" || got[0].Action != "wait" {
			t.Fatalf("safe fallback changed: got %+v err=%v", got, err)
		}
	}
}

func TestReview20261028WellFormedMatchesOldStrictPath(t *testing.T) {
	body := `[
		{"symbol":"ETHUSDT","action":"open_long","leverage":3,"position_size_usd":250.5,
		 "stop_loss":1900.1,"take_profit":2200.2,"confidence":85,"risk_usd":12.5,
		 "reasoning":"1h trend confirmed; risk ~1%, price 2,000; escaped \"quote\" and \\ path",
		 "decision_stage":"READY","entry_quality":81,"blocking_factors":[],
		 "tp_option":2,"exit_mode":"trend","unknown_field":{"ignored":true}},
		{"symbol":"BTCUSDT","action":"hold","reasoning":"trend intact",
		 "no_trade_reason":["await confirmation"],"decision_stage":"IN_POSITION",
		 "management_quality":72,"management_flags":["TREND_INTACT"],
		 "entry_quality":null,"wait_bias":"long","wait_state":"WAIT_TRIGGER",
		 "next_trigger":"Re-check all hard gates after breakout"},
		{"symbol":"SOLUSDT","action":"place_buy_limit","price":150.25,"quantity":1.5,
		 "level_index":2,"order_id":"grid-123","reasoning":"anchor entry","entry_quality":0},
		{"symbol":"BNBUSDT","action":"partial_close_long","close_fraction":0.25,
		 "reasoning":"reduce risk","management_quality":0}
	]`
	for _, response := range []string{
		"<reasoning>Evaluate positions and candidates.</reasoning><decision>" + body + "</decision>",
		"analysis\n```json\n" + body + "\n```",
		"analysis\n" + body,
		"<decision>[]</decision>",
		// Preserve encoding/json's existing key matching and duplicate/null
		// semantics for valid inputs, including exact integers above 2^53.
		`[{"symbol":"ETHUSDT","action":"hold","CONFIDENCE":81,"confidence":82,"confidence":null,"entry_quality":1,"entry_quality":null,"level_index":9007199254740993}]`,
	} {
		want, oldErr := review20261028OldExtractDecisions(response)
		if oldErr != nil {
			t.Fatalf("strict baseline failed: %v", oldErr)
		}
		got, err := extractDecisions(response)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("well-formed output differs from strict baseline:\ngot %+v err=%v\nwant %+v", got, err, want)
		}
	}
}

// Frozen pre-review extraction path: retains the original strict array decoder
// and punctuation replacement for the well-formed compatibility comparison.
func review20261028OldExtractDecisions(response string) ([]Decision, error) {
	s := removeInvisibleRunes(response)
	s = sanitizePlaceholders(s)
	s = strings.TrimSpace(s)
	s = review20261028OldFixMissingQuotes(s)

	var jsonPart string
	if match := reDecisionTag.FindStringSubmatch(s); match != nil && len(match) > 1 {
		inner := match[1]
		// A reasoning section that literally mentions "<decision>" must not
		// widen the payload: the final block is the one after the LAST open tag.
		if i := strings.LastIndex(inner, "<decision>"); i >= 0 {
			inner = inner[i+len("<decision>"):]
		}
		jsonPart = strings.TrimSpace(inner)
		logger.Infof("✓ Extracted JSON using <decision> tag")
	} else if strings.Contains(s, "<decision>") {
		// review 2026-10-20 area4 #3: once the model opened <decision> the
		// payload boundary is fixed. A missing </decision> means the final
		// block was truncated; falling back to a full-text array search would
		// pick up a draft array abandoned inside <reasoning> (e.g. a discarded
		// close_long) and could execute it. Reject instead.
		// Only salvage completed objects from INSIDE the final payload.
		payload := s[strings.LastIndex(s, "<decision>")+len("<decision>"):]
		if salvaged := salvageTruncatedDecisionArray(payload); len(salvaged) > 0 {
			logger.Infof("⚠️  <decision> not closed; salvaged %d complete decisions from the final payload", len(salvaged))
			return salvaged, nil
		}
		return nil, fmt.Errorf("<decision> tag opened but not closed (truncated or malformed final decision block); refusing to search the full text for a decision array")
	} else {
		jsonPart = s
		logger.Infof("⚠️  <decision> tag not found, searching JSON in full text")
	}

	jsonPart = review20261028OldFixMissingQuotes(jsonPart)

	if m := reJSONFence.FindStringSubmatch(jsonPart); m != nil && len(m) > 1 {
		jsonContent := strings.TrimSpace(m[1])
		jsonContent = compactArrayOpen(jsonContent)
		jsonContent = review20261028OldFixMissingQuotes(jsonContent)
		if err := validateJSONFormat(jsonContent); err != nil {
			return nil, fmt.Errorf("JSON format validation failed: %w\nJSON content: %s\nFull response:\n%s", err, jsonContent, response)
		}
		var decisions []Decision
		if err := json.Unmarshal([]byte(jsonContent), &decisions); err != nil {
			return nil, fmt.Errorf("JSON parsing failed: %w\nJSON content: %s", err, jsonContent)
		}
		return decisions, nil
	}

	jsonContent := strings.TrimSpace(reJSONArray.FindString(jsonPart))
	if jsonContent == "" {
		// Max-token truncation amputates the tail of the decision array: the
		// model has usually already emitted several complete decision objects
		// before the cut. Salvage them instead of discarding the whole
		// response to safe-wait — the earlier coins' decisions are real
		// analysis, and a truncated WAIT-heavy board is still directionally
		// correct (positions beyond the cut just keep their default behavior).
		if salvaged := salvageTruncatedDecisionArray(jsonPart); salvaged != nil {
			logger.Warnf("⚠️  [TruncatedSalvage] Decision array was cut off mid-stream (max_tokens); recovered %d complete decisions, later coins default to no-action", len(salvaged))
			return salvaged, nil
		}

		if strings.TrimSpace(jsonPart) == "" {
			logger.Warnf("⚠️  [SafeFallback] AI response is empty after cleanup (%d raw bytes) — upstream returned blank content (check truncation/limits)", len(response))
		} else {
			logger.Infof("⚠️  [SafeFallback] AI didn't output JSON decision, entering safe wait mode")
		}

		// The model's conclusion sits at the END of its reasoning (the beginning
		// is preamble like "Let me analyze this carefully...") — summarize the
		// tail so users see the actual take-away.
		cotSummary := summarizeTail(jsonPart, 240)

		fallbackDecision := Decision{
			Symbol:    "ALL",
			Action:    "wait",
			Reasoning: fmt.Sprintf("Model didn't output structured JSON decision, entering safe wait; summary: %s", cotSummary),
		}

		return []Decision{fallbackDecision}, nil
	}

	jsonContent = compactArrayOpen(jsonContent)
	jsonContent = review20261028OldFixMissingQuotes(jsonContent)

	if err := validateJSONFormat(jsonContent); err != nil {
		return nil, fmt.Errorf("JSON format validation failed: %w\nJSON content: %s\nFull response:\n%s", err, jsonContent, response)
	}

	var decisions []Decision
	if err := json.Unmarshal([]byte(jsonContent), &decisions); err != nil {
		return nil, fmt.Errorf("JSON parsing failed: %w\nJSON content: %s", err, jsonContent)
	}

	return decisions, nil
}

func review20261028OldFixMissingQuotes(jsonStr string) string {
	jsonStr = strings.ReplaceAll(jsonStr, "\u201c", "\"")
	jsonStr = strings.ReplaceAll(jsonStr, "\u201d", "\"")
	jsonStr = strings.ReplaceAll(jsonStr, "\u2018", "'")
	jsonStr = strings.ReplaceAll(jsonStr, "\u2019", "'")

	jsonStr = strings.ReplaceAll(jsonStr, "［", "[")
	jsonStr = strings.ReplaceAll(jsonStr, "］", "]")
	jsonStr = strings.ReplaceAll(jsonStr, "｛", "{")
	jsonStr = strings.ReplaceAll(jsonStr, "｝", "}")
	jsonStr = strings.ReplaceAll(jsonStr, "：", ":")
	jsonStr = strings.ReplaceAll(jsonStr, "，", ",")

	jsonStr = strings.ReplaceAll(jsonStr, "【", "[")
	jsonStr = strings.ReplaceAll(jsonStr, "】", "]")
	jsonStr = strings.ReplaceAll(jsonStr, "〔", "[")
	jsonStr = strings.ReplaceAll(jsonStr, "〕", "]")
	jsonStr = strings.ReplaceAll(jsonStr, "、", ",")

	jsonStr = strings.ReplaceAll(jsonStr, "　", " ")

	return jsonStr
}

func TestReview20261028UnpairedProseQuoteDoesNotCorruptFinalBlock(t *testing.T) {
	in := `<reasoning>He said "maybe and moved on, 价格在“支撑”附近</reasoning>` +
		`<decision>[{"symbol":"BTCUSDT","action":"hold","reasoning":"突破“关键位”失败"}]</decision>`
	got, err := extractDecisions(in)
	if err != nil {
		t.Fatalf("extractDecisions: %v", err)
	}
	if len(got) != 1 || got[0].Action != "hold" || got[0].Reasoning != "突破“关键位”失败" {
		t.Fatalf("got %+v", got)
	}
}
