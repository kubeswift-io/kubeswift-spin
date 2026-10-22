package compiledcache

import "encoding/json"

// ReplaceObject encodes v as a JSON object in which every key of keys that v
// leaves out is null. Sent in a JSON merge patch, it replaces the whole
// object: a merge patch only changes the keys it names, so a field omitted
// because it is empty (a cleared rejection, a zeroed counter) would
// otherwise keep its old value. Every writer of a SpinCompiledCache status
// section uses it.
func ReplaceObject(v any, keys []string) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			out[k] = nil
		}
	}
	return out, nil
}
