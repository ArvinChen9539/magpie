package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// OpenAI takes a reasoning item sent back to it only with its content
// empty ("Invalid 'input[n].content': array too long", #1411) and cannot
// open a seal that is not its own. A client stores what magpie hands it
// and replays it when the conversation goes back to OpenAI, on magpie or
// off it: an item another vendor wrote with its text in it would stick the
// conversation there (#2855, the reporter's rollout: 25 reasoning items a
// DeepSeek phase had left, each with its text in content parts and a
// 38-character encrypted_content of the vendor's own, against the
// thread's own GPT-phase items, which carry none). So an item that carries
// its text goes to the client as one magpie makes: the text a summary,
// nothing sealed.

// reasoningWithText is whether a Responses reasoning item carries its
// reasoning as content parts (reasoning_text), another vendor's shape:
// OpenAI's own carry summaries.
func reasoningWithText(item map[string]any) bool {
	if t, _ := item["type"].(string); t != "reasoning" {
		return false
	}
	parts, _ := item["content"].([]any)
	return len(parts) > 0
}

// magpieMadeReasoning is the reasoning item magpie makes of one a
// Responses upstream wrote with its text in it, nil of one that carries no
// content, which goes as it came. Its text (else its summaries) becomes
// one summary of magpie's; its content and its seal go, the text kept, so
// an upstream that wants its reasoning back is sent it (withReasoningText).
// Its own id is kept, the events of a stream naming the item by it.
func magpieMadeReasoning(item map[string]any) map[string]any {
	if !reasoningWithText(item) {
		return nil
	}
	// the text, else the summaries: a vendor that writes both may write
	// the same words twice (#2855's later rollout: each item's content
	// the summary over again), and the text is what a vendor that wants
	// its reasoning back reads
	text := func(key, typ string) string {
		var b strings.Builder
		parts, _ := item[key].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if t, _ := pm["type"].(string); typ != "" && t != typ {
				continue
			}
			s, _ := pm["text"].(string)
			b.WriteString(s)
		}
		return b.String()
	}
	said := text("content", "reasoning_text")
	if said == "" {
		said = text("summary", "")
	}
	id, _ := item["id"].(string)
	if id == "" {
		id = "rs_" + newID()
	}
	return map[string]any{"id": id, "type": "reasoning", "status": "completed",
		"summary": []map[string]any{{"type": "summary_text", "text": said}}}
}

// magpieMadeReasoningRaw is the item's bytes, made over when the item
// carries its text; id, when not empty, is the id the item's events have
// already carried, which its done event must keep.
func magpieMadeReasoningRaw(raw json.RawMessage, id string) json.RawMessage {
	var it map[string]any
	if decodeJSON(raw, &it) != nil {
		return raw
	}
	made := magpieMadeReasoning(it)
	if made == nil {
		return raw
	}
	if id != "" {
		made["id"] = id
	}
	b, err := json.Marshal(made)
	if err != nil {
		return raw
	}
	return b
}

// reasoningTidy moves relayed Responses reasoning content to summaries,
// including the events that build those parts. When both forms are streamed
// for one item, it keeps the first form and closes gaps from skipped events.
type reasoningTidy struct {
	sse     bool
	buf     []byte
	items   map[int64]*streamedReasoning
	skipped int64
}

type streamedReasoning struct {
	rawFirst bool
	sawRaw   bool
	parts    map[int64]*strings.Builder
}

func (t *reasoningTidy) write(b []byte) []byte {
	t.buf = append(t.buf, b...)
	if !t.sse {
		return nil
	}
	out, rest := tidyEvents(t.buf, t.frame)
	t.buf = rest
	return out
}

func (t *reasoningTidy) flush() []byte {
	b := t.buf
	t.buf = nil
	if !t.sse {
		if out, _, _ := t.rewrite(b); out != nil {
			return out
		}
		return b
	}
	return t.frame(b)
}

func (t *reasoningTidy) frame(frame []byte) []byte {
	if t.skipped == 0 && !bodyHas(frame, `"reasoning"`) && !bodyHas(frame, "reasoning_text") && !bodyHas(frame, "reasoning_summary_") {
		return frame
	}
	data, name, skip := t.rewrite(eventData(frame))
	if skip {
		return nil
	}
	if data == nil {
		return frame
	}
	if name != "" {
		for start := 0; start < len(frame); {
			line, _, _ := bytes.Cut(frame[start:], []byte("\n"))
			if bytes.HasPrefix(line, []byte("event:")) {
				end := start + len(bytes.TrimSuffix(line, []byte("\r")))
				frame = bytes.Join([][]byte{frame[:start], []byte("event: " + name), frame[end:]}, nil)
				break
			}
			start += len(line) + 1
		}
	}
	return withData(frame, data)
}

// rewrite returns changed JSON, a replacement SSE name, and whether to drop
// a frame that belongs to the item's second reasoning form.
func (t *reasoningTidy) rewrite(data []byte) ([]byte, string, bool) {
	var ev map[string]any
	if decodeNumbers(data, &ev) != nil || ev == nil {
		return nil, "", false
	}
	output, hasOutput := reasoningIndex(ev["output_index"])
	seq, hasSeq := reasoningIndex(ev["sequence_number"])
	var name string
	if t.sse && hasOutput {
		if part, rename, ok := reasoningSummaryEvent(ev); ok {
			if !t.keepPart(output, part, rename != "", ev) {
				if hasSeq {
					t.skipped++
				}
				return nil, "", true
			}
			name = rename
			if name != "" {
				ev["type"] = name
				ev["summary_index"] = ev["content_index"]
				delete(ev, "content_index")
				if part, _ := ev["part"].(map[string]any); part != nil {
					part["type"] = "summary_text"
				}
			}
		}
	}
	changed := name != ""
	var streamed *streamedReasoning
	if hasOutput {
		streamed = t.items[output]
	}
	if t.madeOver(ev["item"], streamed) {
		changed = true
	}
	res, _ := ev["response"].(map[string]any)
	if res == nil && ev["object"] == "response" {
		res = ev
	}
	if res != nil {
		out, _ := res["output"].([]any)
		for index, it := range out {
			if t.madeOver(it, t.items[int64(index)]) {
				changed = true
			}
		}
	}
	if hasSeq && t.skipped != 0 {
		ev["sequence_number"] = seq - t.skipped
		changed = true
	}
	if !changed {
		return nil, "", false
	}
	nb, err := marshalPlain(ev)
	if err != nil {
		return nil, "", false
	}
	return nb, name, false
}

func reasoningIndex(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	index, err := number.Int64()
	return index, err == nil && index >= 0
}

func reasoningSummaryEvent(ev map[string]any) (int64, string, bool) {
	typ, _ := ev["type"].(string)
	var name string
	switch typ {
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		index, ok := reasoningIndex(ev["summary_index"])
		return index, "", ok
	case "response.reasoning_text.delta", "response.reasoning_text.done":
		name = strings.Replace(typ, "reasoning_text", "reasoning_summary_text", 1)
	case "response.content_part.added", "response.content_part.done":
		part, _ := ev["part"].(map[string]any)
		if part["type"] != "reasoning_text" {
			return 0, "", false
		}
		name = strings.Replace(typ, "content_part", "reasoning_summary_part", 1)
	default:
		return 0, "", false
	}
	index, ok := reasoningIndex(ev["content_index"])
	return index, name, ok
}

func (t *reasoningTidy) keepPart(output, index int64, raw bool, ev map[string]any) bool {
	item := t.items[output]
	if item == nil {
		if t.items == nil {
			t.items = map[int64]*streamedReasoning{}
		}
		item = &streamedReasoning{rawFirst: raw, parts: map[int64]*strings.Builder{}}
		t.items[output] = item
	}
	item.sawRaw = item.sawRaw || raw
	if item.rawFirst != raw {
		return false
	}
	text := item.parts[index]
	if text == nil {
		text = &strings.Builder{}
		item.parts[index] = text
	}
	if delta, ok := ev["delta"].(string); ok {
		text.WriteString(delta)
	} else {
		part, _ := ev["part"].(map[string]any)
		full, ok := ev["text"].(string)
		if !ok {
			full, ok = part["text"].(string)
		}
		if ok {
			text.Reset()
			text.WriteString(full)
		}
	}
	return true
}

func (item *streamedReasoning) summary() []rText {
	parts := make([]rText, 0, len(item.parts))
	for index := int64(0); index < int64(len(item.parts)); index++ {
		text := item.parts[index]
		if text == nil {
			return nil
		}
		parts = append(parts, rText{Type: "summary_text", Text: text.String()})
	}
	return parts
}

// madeOver makes item, a reasoning item with its text in it, one of
// magpie's, in place, and reports whether it was.
func (t *reasoningTidy) madeOver(item any, streamed *streamedReasoning) bool {
	im, _ := item.(map[string]any)
	made := magpieMadeReasoning(im)
	if streamed != nil && im["type"] == "reasoning" && (streamed.sawRaw || made != nil) {
		if summary := streamed.summary(); summary != nil {
			if made == nil {
				made = map[string]any{"type": "reasoning", "id": im["id"], "status": "completed"}
			}
			made["summary"] = summary
		}
	}
	if made == nil {
		return false
	}
	if t.sse {
		if status, ok := im["status"].(string); ok && status != "" {
			made["status"] = status
		}
	}
	for k := range im {
		delete(im, k)
	}
	for k, v := range made {
		im[k] = v
	}
	return true
}
