package svc

import (
	"bytes"
	"encoding/xml"
	"unicode/utf16"
)

// UpdateTask is the Windows scheduled task that installs staged updates.
// The service's own account can't write Program Files (on purpose), so a
// task running as SYSTEM does it: every 5 minutes it runs apply-update,
// which does nothing unless the bridge has staged a verified update and
// marked it ready.
const UpdateTask = `SIERRA\meshbridge update`

// UpdateTaskRevision changes whenever updateTaskXML does. Install records
// it beside the config, and apply-update re-registers a task from an older
// revision, so bridges installed before a change pick it up on their own.
//
// Revision 2: run on battery power. schtasks /Create's defaults skip a task
// while the computer is on battery, so a laptop bridge that was unplugged
// downloaded updates and never installed them.
const UpdateTaskRevision = "2"

// UpdateTaskMarker is the file, beside the config, that records the
// revision of the registered task.
const UpdateTaskMarker = "update-task"

// updateTaskXML is the task's definition, for schtasks /Create /XML, which
// reads UTF-16. command and arguments are the action's command line, quoted
// for Windows.
//
// Unlike schtasks' defaults it runs on battery and keeps running if the
// computer goes onto battery, catches up after sleep (StartWhenAvailable),
// and is stopped after 10 minutes: apply-update takes a minute at most, and
// a hung run would otherwise block every later one for 3 days.
func updateTaskXML(command, arguments string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	doc := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Installs SIERRA meshbridge updates that the bridge has downloaded and verified. https://github.com/sierragridteam/sierra-meshrelay</Description>
  </RegistrationInfo>
  <Triggers>
    <TimeTrigger>
      <Repetition>
        <Interval>PT5M</Interval>
        <StopAtDurationEnd>false</StopAtDurationEnd>
      </Repetition>
      <StartBoundary>2020-01-01T00:00:00</StartBoundary>
      <Enabled>true</Enabled>
    </TimeTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT10M</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(command) + `</Command>
      <Arguments>` + esc(arguments) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
	return utf16LE(doc)
}

// utf16LE encodes s as UTF-16 little-endian with a byte-order mark.
func utf16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}
