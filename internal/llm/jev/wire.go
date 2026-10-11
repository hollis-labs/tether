package jev

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/tether/internal/llm"
)

// JEV reads option order (it leans toward the first option), and Go maps lose
// order, so every ordered structure is marshaled by hand.

// buildBody encodes the request body {state, model, questions}.
func buildBody(model string, dec *llm.DecisionRequest) ([]byte, error) {
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	state, err := json.Marshal(dec.State)
	if err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}
	modelJSON, _ := json.Marshal(model)

	var qs bytes.Buffer
	qs.WriteByte('{')
	for i, q := range dec.Questions {
		if i > 0 {
			qs.WriteByte(',')
		}
		key, _ := json.Marshal(q.Key)
		qs.Write(key)
		qs.WriteByte(':')
		body, err := encodeQuestion(q)
		if err != nil {
			return nil, err
		}
		qs.Write(body)
	}
	qs.WriteByte('}')

	var out bytes.Buffer
	out.WriteString(`{"state":`)
	out.Write(state)
	out.WriteString(`,"model":`)
	out.Write(modelJSON)
	out.WriteString(`,"questions":`)
	out.Write(qs.Bytes())
	out.WriteByte('}')
	return out.Bytes(), nil
}

func encodeQuestion(q llm.DecisionQuestion) ([]byte, error) {
	instructions, _ := json.Marshal(q.Instructions)
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"type":%q,"instructions":%s`, string(q.Type), instructions)
	switch q.Type {
	case llm.QuestionNoul:
		if q.True != "" || q.False != "" {
			b.WriteString(`,"criteria":{`)
			sep := ""
			if q.True != "" {
				t, _ := json.Marshal(q.True)
				b.WriteString(`"true":`)
				b.Write(t)
				sep = ","
			}
			if q.False != "" {
				f, _ := json.Marshal(q.False)
				b.WriteString(sep + `"false":`)
				b.Write(f)
			}
			b.WriteByte('}')
		}
	case llm.QuestionChoice:
		b.WriteString(`,"criteria":{`)
		for i, o := range q.Options {
			if i > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(o.Key)
			b.Write(k)
			b.WriteByte(':')
			if o.Description == "" {
				b.WriteString("null")
			} else {
				d, _ := json.Marshal(o.Description)
				b.Write(d)
			}
		}
		b.WriteByte('}')
	case llm.QuestionScore:
		levels, _ := json.Marshal(q.Levels)
		b.WriteString(`,"criteria":`)
		b.Write(levels)
	default:
		return nil, fmt.Errorf("question %q: unsupported type %q", q.Key, q.Type)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          *float64           `json:"noul"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// translateAnswers maps JEV answers to provider-neutral ones, requiring one
// well-formed answer per question asked.
func translateAnswers(dec *llm.DecisionRequest, wire *wireResponse) (map[string]llm.DecisionAnswer, error) {
	out := make(map[string]llm.DecisionAnswer, len(dec.Questions))
	for _, q := range dec.Questions {
		a, ok := wire.Answers[q.Key]
		if !ok {
			return nil, fmt.Errorf("response missing answer for question %q", q.Key)
		}
		if a.Type != string(q.Type) {
			return nil, fmt.Errorf("answer for %q has type %q, want %q", q.Key, a.Type, q.Type)
		}
		ans := llm.DecisionAnswer{Type: q.Type}
		switch q.Type {
		case llm.QuestionNoul:
			if a.Noul == nil {
				return nil, fmt.Errorf("noul answer for %q has no value", q.Key)
			}
			ans.Noul = *a.Noul
			// JEV documents no confidence for a noul; if a field ever
			// appears, pass it through rather than invent one.
			ans.Confidence = a.Confidence
		case llm.QuestionChoice:
			valid := false
			for _, o := range q.Options {
				if o.Key == a.Choice {
					valid = true
					break
				}
			}
			if !valid {
				return nil, fmt.Errorf("choice answer for %q is not one of its options", q.Key)
			}
			ans.Choice = a.Choice
			ans.Probabilities = a.Probabilities
			ans.Confidence = a.Confidence
		case llm.QuestionScore:
			if a.Score == nil {
				return nil, fmt.Errorf("score answer for %q has no value", q.Key)
			}
			ans.Score = *a.Score
			ans.Probabilities = a.Probabilities
			ans.Confidence = a.Confidence
		}
		out[q.Key] = ans
	}
	return out, nil
}
