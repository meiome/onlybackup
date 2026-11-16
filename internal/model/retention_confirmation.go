package model

// Administrative confirmations describe the action being authorized. The CLI
// asks for the exact text; scripts and local API callers must supply it too.
const (
	ConfirmRetentionEnable          = "ABILITA"
	ConfirmRetentionPause           = "SOSPENDI"
	ConfirmRetentionResume          = "RIPRENDI"
	ConfirmRetentionResumeWhenReady = "PRENOTA"
)

// ResumeHint tells the administrator what to run when a resolved incident left
// deletion suspended. It is shown in the resolution mail and in the output of
// monitoring acknowledge/exclude, so the required step is never silent.
const ResumeHint = "Cancellazione sospesa: esegui onlybackup-admin retention resume (con controllo recente regolare) per riattivarla. Nessuna cancellazione parte finché non lo esegui."

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
