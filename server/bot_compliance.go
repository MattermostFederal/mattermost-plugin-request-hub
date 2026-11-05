package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// defaultBotComplianceText is the statement a bot requester accepts when
// BotComplianceText is blank.
const defaultBotComplianceText = "By submitting this request I confirm that I will not use this bot for unauthorized, abusive, or harmful activity. " +
	"I am responsible for all actions taken with this bot account, its tokens, and any webhooks associated with this request. " +
	"I understand that activity may be audited and that misuse may result in revocation of access and other actions under organizational policy."

const (
	botComplianceRequiredMsg = "Accept the bot compliance statement to submit this request."
	botComplianceChangedMsg  = "The bot compliance statement changed after you opened the form. Read it again and resubmit."
	botComplianceMCPMsg      = "Bot requests through an agent are off while the bot compliance statement is required. Use the Request a Bot Account form or the bot slash command."

	// complianceHashShort is how many characters of the hash name the
	// statement in notes and cards.
	complianceHashShort = 8
)

// botCompliance records the requester's acceptance of the compliance
// statement: when, and the exact text, so a later change to the setting
// doesn't rewrite what they accepted.
type botCompliance struct {
	AcceptedAt int64  `json:"accepted_at"`
	TextSHA256 string `json:"text_sha256"`
	Text       string `json:"text"`
}

// BotComplianceRequired reports whether bot requesters must accept the
// compliance statement.
func (c *configuration) BotComplianceRequired() bool {
	return c.BotComplianceEnabled
}

// BotComplianceText returns the statement bot requesters accept: the
// setting, or the built-in default when it is blank.
func (c *configuration) BotComplianceText() string {
	if text := strings.TrimSpace(c.BotComplianceStatement); text != "" {
		return text
	}
	return defaultBotComplianceText
}

// complianceHash is the SHA-256 of a compliance statement, in hex.
func complianceHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func shortHash(hash string) string {
	if len(hash) > complianceHashShort {
		return hash[:complianceHashShort]
	}
	return hash
}

// checkBotCompliance returns the acceptance to store with a bot request, or
// nil when no acceptance is required. shownHash, when set, is the hash of
// the statement the requester was shown; a different current statement is
// refused so nobody accepts text they never saw.
func checkBotCompliance(c *configuration, accepted bool, shownHash string) (*botCompliance, error) {
	if !c.BotComplianceRequired() {
		return nil, nil
	}
	if !accepted {
		return nil, errors.New(botComplianceRequiredMsg)
	}
	text := c.BotComplianceText()
	hash := complianceHash(text)
	if shownHash = strings.TrimSpace(shownHash); shownHash != "" && shownHash != hash {
		return nil, errors.New(botComplianceChangedMsg)
	}
	return &botCompliance{AcceptedAt: model.GetMillis(), TextSHA256: hash, Text: text}, nil
}

// complianceAuditDetail is what a submission records about the acceptance.
func complianceAuditDetail(bc *botCompliance) map[string]any {
	if bc == nil {
		return nil
	}
	return map[string]any{
		"compliance_accepted":    true,
		"compliance_accepted_at": bc.AcceptedAt,
		"compliance_text_sha256": bc.TextSHA256,
	}
}

// complianceNote is the short line naming the accepted statement.
func complianceNote(bc *botCompliance) string {
	return fmt.Sprintf("accepted the bot compliance statement (sha256 %s)", shortHash(bc.TextSHA256))
}

// slashComplianceText is the reply to a bot slash command sent without
// --accept-compliance while the statement is required.
func slashComplianceText(c *configuration) string {
	return "Bot requests need you to accept this statement:\n\n> " +
		strings.ReplaceAll(c.BotComplianceText(), "\n", "\n> ") +
		"\n\nTo accept it, run the command again with `--accept-compliance`."
}

// botFormDTO is what the webapp's bot form needs from the server.
type botFormDTO struct {
	Compliance botComplianceDTO `json:"compliance"`
}

type botComplianceDTO struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// handleBotForm serves the bot form's settings: whether the compliance
// statement is required, its text and its hash.
func (p *Plugin) handleBotForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	c := p.getConfiguration()
	out := botFormDTO{}
	if c.BotComplianceRequired() {
		text := c.BotComplianceText()
		out.Compliance = botComplianceDTO{Enabled: true, Text: text, SHA256: complianceHash(text)}
	}
	writeJSON(w, out)
}
