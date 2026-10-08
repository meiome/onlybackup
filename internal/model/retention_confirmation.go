package model

// Administrative confirmations describe the action being authorized. The CLI
// asks for the exact text; scripts and local API callers must supply it too.
const (
	ConfirmRetentionEnable          = "ABILITA"
	ConfirmRetentionPause           = "SOSPENDI"
	ConfirmRetentionResume          = "RIPRENDI"
	ConfirmRetentionResumeWhenReady = "PRENOTA"
)

func ConfirmAutomationSetup(mode string) string {
	switch mode {
	case "auto":
		return "CONFIGURA AUTO"
	case "manual":
		return "CONFIGURA MANUAL"
	default:
		return "CONFIGURA"
	}
}
