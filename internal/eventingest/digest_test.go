package eventingest

import (
	"encoding/json"
	"testing"
)

func TestJCSCanonicalForm(t *testing.T) {
	// RFC 8785 sections 3.2.2 to 3.2.3 and appendix B number samples.
	for in, want := range map[string]string{
		`{"b":2,"a":1}`: `{"a":1,"b":2}`,
		`{ "peach": "This sorting order", "péché": "is wrong", "pêche": "according to French", "sin": "Punishable" }`:                                                                                              `{"peach":"This sorting order","péché":"is wrong","pêche":"according to French","sin":"Punishable"}`,
		`{"numbers":[333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001, -0, 1e21, 1e20, 0.000001, 0.0000001],"string":"\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\","literals":[null,true,false]}`: `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27,0,1e+21,100000000000000000000,0.000001,1e-7],"string":"€$\u000f\nA'B\"\\\\"}`,
	} {
		var v map[string]any
		d := json.NewDecoder(stringsReader(in))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		var b bytesBuffer
		if err := jcs(&b, v); err != nil {
			t.Fatal(err)
		}
		if b.String() != want {
			t.Errorf("jcs(%s)\n got %s\nwant %s", in, b.String(), want)
		}
	}
}

func TestDigestIgnoresOnlyServerOwnedFields(t *testing.T) {
	base := `{"id":"evt_1","kind":"k","app":"a","time":"2026-10-20T00:00:00Z","scope":{"project_id":"p"},"corr":{"correlation_id":"c"},"class":{"sensitivity":"internal"},"process":{"pid":1},"payload":{"a":1}}`
	d0, err := eventDigest([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	same := []string{
		` { "payload": {"a": 1}, "process": {"pid": 1}, "class": {"sensitivity": "internal"}, "corr": {"correlation_id": "c"}, "scope": {"project_id": "p"}, "time": "2026-10-20T00:00:00Z", "app": "a", "kind": "k", "id": "evt_1" } `,
		`{"id":"evt_1","kind":"k","app":"a","time":"2026-10-20T00:00:00Z","scope":{"project_id":"p"},"corr":{"correlation_id":"c"},"class":{"sensitivity":"internal"},"process":{"pid":1},"payload":{"a":1},"recorded_at":"x","cursor":"9","actor":{"kind":"user"}}`,
	}
	for _, s := range same {
		if d, _ := eventDigest([]byte(s)); d != d0 {
			t.Errorf("server-owned or layout difference changed the digest: %s", s)
		}
	}
	diff := map[string]string{
		"kind":    `"kind":"k2"`,
		"app":     `"app":"b"`,
		"time":    `"time":"2026-10-20T00:00:01Z"`,
		"scope":   `"scope":{"project_id":"q"}`,
		"corr":    `"corr":{"correlation_id":"d"}`,
		"class":   `"class":{"sensitivity":"private"}`,
		"process": `"process":{"pid":2}`,
		"payload": `"payload":{"a":2}`,
	}
	for name, repl := range diff {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(base), &m)
		var r map[string]json.RawMessage
		_ = json.Unmarshal([]byte("{"+repl+"}"), &r)
		for k, v := range r {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		if d, _ := eventDigest(b); d == d0 {
			t.Errorf("a different %s kept the digest", name)
		}
	}
}
