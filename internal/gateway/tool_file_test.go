package gateway

import (
	"strings"
	"testing"
)

func TestTranslatedToolFiles(t *testing.T) {
	const reference = "https://example.invalid/doc.pdf"
	const csv = "YSxiCg=="
	for _, fixture := range []struct {
		name  string
		read  func([]byte) (*Request, error)
		body  string
		notes []string
	}{
		{"messages", func(body []byte) (*Request, error) { return readAnthropic(body, false, false) },
			`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"read the file"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_read","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_read","content":[{"type":"text","text":"read it"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64 + `"}},{"type":"document","source":{"type":"url","url":"` + reference + `"}},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain document words"}}]}]}]}`, []string{reference, "plain document words"}},
		{"responses", parseResponses,
			`{"model":"m","input":[{"role":"user","content":"read the file"},{"type":"function_call","call_id":"call_read","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_read","output":[{"type":"input_text","text":"read it"},{"type":"input_file","filename":"doc.pdf","file_data":"` + pdfB64 + `"},{"type":"input_file","file_url":"` + reference + `"}]}]}`, []string{reference}},
		{"chat", parseChat,
			`{"model":"m","messages":[{"role":"user","content":"read the file"},{"role":"assistant","tool_calls":[{"id":"call_read","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_read","content":[{"type":"text","text":"read it"},{"type":"file","file":{"filename":"doc.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}},{"type":"file","file":{"filename":"notes.csv","file_data":"data:text/csv;base64,` + csv + `"}},{"type":"file","file":{"filename":"doc.pdf","file_id":"file-vendor-only"}},{"type":"file","file":{"filename":"bad.pdf","file_data":"not-base64!"}}]}]}`, []string{"file-vendor-only", "[attachment text/csv]"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			request, err := fixture.read([]byte(fixture.body))
			if err != nil {
				t.Fatal(err)
			}
			gemini, err := buildGemini(request, "m")
			if err != nil {
				t.Fatal(err)
			}
			vertex, err := buildVertex(request, "m")
			if err != nil {
				t.Fatal(err)
			}
			const pdfPart = `"inlineData":{"data":"` + pdfB64 + `","mimeType":"application/pdf"}`
			for name, built := range map[string][]byte{"Gemini": gemini, "Vertex": vertex} {
				body := string(built)
				response, inline := strings.Index(body, `"functionResponse"`), strings.Index(body, pdfPart)
				if response < 0 || inline <= response || strings.Contains(body, csv) || strings.Contains(body, "not-base64!") || strings.Contains(body, `"fileData"`) {
					t.Errorf("%s lost the PDF after the result or mishandled another attachment: %s", name, body)
				} else if name == "Vertex" && !strings.Contains(body[response:inline], `"role":"user"`) {
					t.Errorf("Vertex did not put the PDF in a separate user turn: %s", body)
				}
			}
			for name, built := range map[string][]byte{
				"chat":                 buildChat(request, "m", "api.example.com", false),
				"responses":            buildResponses(request, "m", "api.example.com", false),
				"messages":             buildAnthropic(request, "m"),
				"antigravity (Claude)": buildCodeAssistSent(request, "claude-sonnet-4-6", "antigravity"),
			} {
				body := string(built)
				if !strings.Contains(body, "read it") || !strings.Contains(body, "[attachment application/pdf]") || strings.Contains(body, pdfB64) || strings.Contains(body, csv) || strings.Contains(body, "not-base64!") {
					t.Errorf("%s did not preserve the result as text: %s", name, body)
				}
				for _, note := range fixture.notes {
					if !strings.Contains(body, note) {
						t.Errorf("%s lost %q in %s", name, note, body)
					}
				}
			}
		})
	}
}
