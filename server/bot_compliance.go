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

// defaultBotComplianceText is the built-in Terms and Conditions a bot
// requester must accept when BotComplianceStatement is blank. System Console
// can replace this text; the request stores the exact wording that was shown.
const defaultBotComplianceText = `MATTERMOST REQUEST HUB — BOT ACCOUNT TERMS AND CONDITIONS

By requesting a bot account you agree to the following:

1. Authorized use only. You will not use this bot, its access tokens, or any associated webhooks for unauthorized, abusive, harmful, deceptive, or illegal activity.

2. Responsibility. You are responsible for all actions taken with this bot account, including posts, API calls, file access, and webhook traffic, whether performed by you or by systems you connect to the bot.

3. Credentials. You will protect tokens and webhook URLs, store them in an approved secrets manager where required, and revoke or rotate them if they may have been exposed. Delivery DMs that contain secrets may auto-delete; save secrets before they disappear.

4. Least privilege. The bot remains a regular Mattermost user (not Team Admin or System Admin). You will not attempt to escalate the bot's role outside approved organizational process.

5. Audit and review. Activity may be audited. Request Hub may retain declaration fields, compliance acceptance, and related history. Misuse may result in revocation of access and other actions under organizational policy.

6. Accuracy. The purpose, hosting, architecture, and integration details you declare are accurate to the best of your knowledge.

If your organization has published a replacement statement in System Console, that statement controls instead of this built-in text.`

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

// botFormDTO is what the webapp's bot form needs from the server: the
// Terms and Conditions statement shown in the submit popup.
type botFormDTO struct {
	Compliance botComplianceDTO `json:"compliance"`
}

type botComplianceDTO struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// handleBotForm serves whether the compliance statement is required and
// its text/hash for the Terms and Conditions popup on submit.
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
