package tools

import (
	"regexp"
	"strings"
)

// The PowerShell and cmd.exe forms of the hardline: on Windows the exec tool
// runs PowerShell, so the same last line of defense needs their spellings.
// They are checked on every OS (a command may be meant for a Windows node),
// and are specific enough not to match ordinary Unix commands.
var (
	// psFetchToShell: iwr https://x | iex, irm https://x | Invoke-Expression.
	psFetchToShell = regexp.MustCompile(`(?i)\b(iwr|irm|curl|wget|Invoke-WebRequest|Invoke-RestMethod)\b([^|;\n]*)\|\s*(iex|Invoke-Expression)\b`)
	// psShellOfFetch: iex (iwr ...), Invoke-Expression (New-Object Net.WebClient).DownloadString(...).
	psShellOfFetch = regexp.MustCompile(`(?i)\b(?:iex|Invoke-Expression)\b\s*\(?\s*(?:\(\s*)?(?:iwr|irm|Invoke-WebRequest|Invoke-RestMethod|New-Object\s+(?:System\.)?Net\.WebClient)\b([^\n]*)`)
	// winRecursiveDelete: Remove-Item -Recurse (or rm/del/rd/rmdir with
	// recursion) whose target is a drive root, the user profile or Windows.
	winRecursiveDelete = regexp.MustCompile(`(?i)\b(?:Remove-Item|ri|rm|rmdir|rd|del|erase)\b([^;|&\n]*)`)
	winRecurseFlag     = regexp.MustCompile(`(?i)(?:^|\s)(?:-r(?:ecurse)?\b|/s\b)`)
	winDangerTarget    = regexp.MustCompile(`(?i)(?:^|[\s"'=])(?:[a-z]:\\?(?:\*|\\\*)?|\$env:(?:USERPROFILE|SystemRoot|windir|SystemDrive|HOMEDRIVE)\\?(?:\*)?|%(?:USERPROFILE|SystemRoot|windir|SystemDrive)%\\?(?:\*)?|~\\?(?:\*)?|[a-z]:\\(?:Windows|Users|Program Files(?: \(x86\))?)\\?(?:\*)?)["']?(?:\s|$)`)
	// winFormat: format C:, Format-Volume, Clear-Disk, Initialize-Disk, diskpart clean.
	winFormat = regexp.MustCompile(`(?i)(?:^|[;&|\s])(?:format(?:\.com)?\s+[a-z]:|Format-Volume\b|Clear-Disk\b|Initialize-Disk\b|diskpart\b)`)
)

// hardlineWindows returns why a PowerShell or cmd.exe command is on the
// blocklist, or "".
func hardlineWindows(cmd string) string {
	for _, m := range psFetchToShell.FindAllStringSubmatch(cmd, -1) {
		if remoteFetch(m[2]) {
			return "piping a download from a remote host into Invoke-Expression"
		}
	}
	for _, m := range psShellOfFetch.FindAllStringSubmatch(cmd, -1) {
		if remoteFetch(m[1]) {
			return "running a script downloaded from a remote host"
		}
	}
	if winFormat.MatchString(cmd) {
		return "formatting or wiping a disk"
	}
	for _, m := range winRecursiveDelete.FindAllStringSubmatch(cmd, -1) {
		args := m[1]
		if winRecurseFlag.MatchString(args) && winDangerTarget.MatchString(strings.TrimSpace(args)+" ") {
			return "recursively deleting a drive, Windows or the user profile"
		}
	}
	return ""
}
