// Package dsn contains the utilities used for dsn message (DSN) generation.
//
// It implements RFC 3464 and RFC 3462.
package dsn

import (
	"errors"
	"fmt"
	"io"
	"text/template"
	"time"

	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-smtp"
)

type ReportingMTAInfo struct {
	ReportingMTA    string
	ReceivedFromMTA string

	// Message sender address, included as 'X-Maddy-Sender: rfc822; ADDR' field.
	XSender string

	// Message identifier, included as 'X-Maddy-MsgId: MSGID' field.
	XMessageID string

	// Time when message was enqueued for delivery by Reporting MTA.
	ArrivalDate time.Time

	// Time when message delivery was attempted last time.
	LastAttemptDate time.Time
}

func (info ReportingMTAInfo) WriteTo(w io.Writer) error {
	// DSN format uses structure similar to MIME header, so we reuse
	// MIME generator here.
	h := textproto.Header{}

	if info.ReportingMTA == "" {
		return errors.New("dsn: Reporting-MTA field is mandatory")
	}
	h.Add("Reporting-MTA", "dns; "+info.ReportingMTA)

	if info.ReceivedFromMTA != "" {
		h.Add("Received-From-MTA", "dns; "+info.ReceivedFromMTA)
	}

	if info.XSender != "" {
		h.Add("X-Maddy-Sender", "rfc822; "+info.XSender)
	}
	if info.XMessageID != "" {
		h.Add("X-Maddy-MsgID", info.XMessageID)
	}

	if !info.ArrivalDate.IsZero() {
		h.Add("Arrival-Date", info.ArrivalDate.Format("Mon, 2 Jan 2006 15:04:05 -0700"))
	}
	if !info.ArrivalDate.IsZero() {
		h.Add("Last-Attempt-Date", info.ArrivalDate.Format("Mon, 2 Jan 2006 15:04:05 -0700"))
	}

	return textproto.WriteHeader(w, h)
}

type Action string

const (
	ActionFailed    Action = "failed"
	ActionDelayed   Action = "delayed"
	ActionDelivered Action = "delivered"
	ActionRelayed   Action = "relayed"
	ActionExpanded  Action = "expanded"
)

type RecipientInfo struct {
	FinalRecipient string
	RemoteMTA      string

	Action Action
	Status smtp.EnhancedCode

	// DiagnosticCode is the error that will be returned to the sender.
	DiagnosticCode error
}

func (info RecipientInfo) WriteTo(w io.Writer) error {
	// DSN format uses structure similar to MIME header, so we reuse
	// MIME generator here.
	h := textproto.Header{}

	if info.FinalRecipient == "" {
		return errors.New("dsn: Final-Recipient is required")
	}
	h.Add("Final-Recipient", "rfc822; "+info.FinalRecipient)
	if info.Action == "" {
		return errors.New("dsn: Action is required")
	}
	h.Add("Action", string(info.Action))
	if info.Status[0] == 0 {
		return errors.New("dsn: Status is required")
	}
	h.Add("Status", fmt.Sprintf("%d.%d.%d", info.Status[0], info.Status[1], info.Status[2]))

	if smtpErr, ok := info.DiagnosticCode.(*smtp.SMTPError); ok {
		h.Add("Diagnostic-Code", fmt.Sprintf("smtp; %d %d.%d.%d %s",
			smtpErr.Code, smtpErr.EnhancedCode[0], smtpErr.EnhancedCode[1], smtpErr.EnhancedCode[2],
			smtpErr.Message))
	} else {
		h.Add("Diagnostic-Code", "X-Maddy; "+info.DiagnosticCode.Error())
	}

	if info.RemoteMTA != "" {
		h.Add("Remote-MTA", "dns; "+info.RemoteMTA)
	}

	return textproto.WriteHeader(w, h)
}

type Envelope struct {
	MsgID string
	From  string
	To    string
}

// GenerateDSN is a top-level function that should be used for generation of the DSNs.
//
// DSN header will be returned, body itself will be written to outWriter.
func GenerateDSN(envelope Envelope, mtaInfo ReportingMTAInfo, rcptsInfo []RecipientInfo, failedHeader textproto.Header, outWriter io.Writer) (textproto.Header, error) {
	partWriter := textproto.NewMultipartWriter(outWriter)

	reportHeader := textproto.Header{}
	reportHeader.Add("Date", time.Now().Format("Mon, 2 Jan 2006 15:04:05 -0700"))
	reportHeader.Add("Message-Id", envelope.MsgID)
	reportHeader.Add("Content-Transfer-Encoding", "8bit")
	reportHeader.Add("Content-Type", "multipart/report; report-type=delivery-status; boundary="+partWriter.Boundary())
	reportHeader.Add("MIME-Version", "1.0")
	reportHeader.Add("Auto-Submitted", "auto-replied")
	reportHeader.Add("To", envelope.To)
	reportHeader.Add("From", envelope.From)
	reportHeader.Add("Subject", "Undelivered Mail Returned to Sender")

	defer partWriter.Close()

	if err := writeHumanReadablePart(partWriter, envelope, mtaInfo, rcptsInfo); err != nil {
		return textproto.Header{}, err
	}
	if err := writeMachineReadablePart(partWriter, mtaInfo, rcptsInfo); err != nil {
		return textproto.Header{}, err
	}
	return reportHeader, writeHeader(partWriter, failedHeader)

}

func writeHeader(w *textproto.MultipartWriter, header textproto.Header) error {
	partHeader := textproto.Header{}
	partHeader.Add("Content-Description", "Undelivered message header")
	partHeader.Add("Content-Type", "message/rfc822-headers")
	partHeader.Add("Content-Transfer-Encoding", "8bit")
	headerWriter, err := w.CreatePart(partHeader)
	if err != nil {
		return err
	}
	return textproto.WriteHeader(headerWriter, header)
}

func writeMachineReadablePart(w *textproto.MultipartWriter, mtaInfo ReportingMTAInfo, rcptsInfo []RecipientInfo) error {
	machineHeader := textproto.Header{}
	machineHeader.Add("Content-Type", "message/delivery-status")
	machineHeader.Add("Content-Description", "Delivery report")
	machineWriter, err := w.CreatePart(machineHeader)
	if err != nil {
		return err
	}

	// WriteTo will add an empty line after output.
	if err := mtaInfo.WriteTo(machineWriter); err != nil {
		return err
	}

	for _, rcpt := range rcptsInfo {
		if err := rcpt.WriteTo(machineWriter); err != nil {
			return err
		}
	}
	return nil
}

// failedText is the text of the human-readable part of DSN.
var failedText = template.Must(template.New("dsn-text").Parse(`
This is the mail delivery system at {{.ReportingMTA}}. 

Unfortunately, your message could not be delivered to one or more
recipients. The usual cause of this problem is invalid 
recipient address or maintenance at the recipient side.

Contact the postmaster for further assistance, provide the Message ID (below):

Message ID: {{.XMessageID}}
Arrival: {{.ArrivalDate}}
Last delivery attempt: {{.LastAttemptDate}}

`))

func writeHumanReadablePart(w *textproto.MultipartWriter, envelope Envelope, mtaInfo ReportingMTAInfo, rcptsInfo []RecipientInfo) error {
	humanHeader := textproto.Header{}
	humanHeader.Add("Content-Transfer-Encoding", "8bit")
	humanHeader.Add("Content-Type", "text/plain; encoding=utf-8")
	humanHeader.Add("Content-Description", "Notification")
	humanWriter, err := w.CreatePart(humanHeader)
	if err != nil {
		return err
	}
	if err := failedText.Execute(humanWriter, mtaInfo); err != nil {
		return err
	}

	for _, rcpt := range rcptsInfo {
		if _, err := fmt.Fprintf(humanWriter, "Delivery to %s (%s) failed with error: %v\n", rcpt.FinalRecipient, rcpt.RemoteMTA, rcpt.DiagnosticCode); err != nil {
			return err
		}
	}

	return nil
}
