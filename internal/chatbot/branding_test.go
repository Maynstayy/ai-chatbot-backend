package chatbot

import (
	"strings"
	"testing"
	"time"
)

// What the model is told and shown must carry the SKYMCM brand only.
func TestPromptAndToolsUseSKYMCMBranding(t *testing.T) {
	texts := []string{systemPrompt(time.Now(), Input{})}
	for _, tool := range append(publicTools(), bookingTools()...) {
		texts = append(texts, tool.Description)
	}
	for _, s := range texts {
		if strings.Contains(strings.ToLower(s), "maynstay") {
			t.Errorf("MaynStay branding in model-facing text: %.120s", s)
		}
	}
	if !strings.HasPrefix(basePrompt, "You are the SKYMCM assistant") {
		t.Errorf("prompt opening changed: %.60s", basePrompt)
	}
}
