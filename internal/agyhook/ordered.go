package agyhook

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// orderedObject is a top-level JSON object whose key order survives a rewrite,
// so editing one key does not reshuffle the user's settings file.
type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseOrdered(b []byte) (*orderedObject, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("settings.json 최상위가 객체가 아님")
	}
	o := &orderedObject{vals: map[string]json.RawMessage{}}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("settings.json 키 해석 실패")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if _, dup := o.vals[key]; !dup {
			o.keys = append(o.keys, key)
		}
		o.vals[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *orderedObject) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *orderedObject) set(k string, v json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *orderedObject) del(k string) {
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

func (o *orderedObject) marshalIndent() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range o.keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		var vb bytes.Buffer
		if err := json.Indent(&vb, o.vals[k], "  ", "  "); err != nil {
			return nil, err
		}
		b.WriteString("  ")
		b.Write(kb)
		b.WriteString(": ")
		b.Write(vb.Bytes())
		if i < len(o.keys)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}
