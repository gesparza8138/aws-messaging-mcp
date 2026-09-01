package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/awsclients"
	"github.com/gesparza8138/aws-messaging-mcp/internal/guardrails"
	"github.com/gesparza8138/aws-messaging-mcp/internal/httpapi"
	"github.com/gesparza8138/aws-messaging-mcp/internal/mimebuild"
	"github.com/gesparza8138/aws-messaging-mcp/internal/schemas"
)

// sesMaxMessageBytes is SES's documented ceiling for one message, applied to
// the message the server assembles itself.
//
// Deliberately not EmailMaxRawBytes (10 MB by default), which keeps metering
// the *decoded* attachment bytes exactly as it does today: base64 inflates by
// 4/3 before headers and boundaries, so 10 MB of attachments assembles to
// ~13.7 MB, and measuring the assembled message against the attachment budget
// would start refusing sends that work today.
const sesMaxMessageBytes = 40 << 20

// Bounds for the walk of a caller-supplied Content.Raw message, whose bytes
// are untrusted input: a 10 MB message can hold millions of empty parts, and
// describing the first rawWalkMaxParts of it is more useful than refusing to
// describe it. A message the server assembled needs neither bound — its part
// list comes back from Assemble.
const (
	rawWalkMaxDepth = 10
	rawWalkMaxParts = 200
)

// ContentDigest is a SHA-256 over one decoded binary part, so a client can
// confirm the bytes the server received are the bytes it meant to send
// (the DryRun echo re-encodes them, a digest does not).
type ContentDigest struct {
	Part   string `json:"part"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// ServerMetadata accompanies every send-tool result (PRD 5.1 rule 4).
//
// MimeStructure describes the message ses_send_email is about to send — the
// part tree and each part's headers, never a body — so an image that does not
// render can be diagnosed from the call itself instead of by sending a real
// message to a real person and asking what they saw. It is flat by necessity;
// see the note on mimebuild.Part. Only ses_send_email ever fills it, and only
// when there is an assembled or caller-supplied message to describe.
type ServerMetadata struct {
	Guardrails     []guardrails.Decision `json:"guardrails"`
	DryRun         bool                  `json:"dry_run"`
	ContentDigests []ContentDigest       `json:"content_digests,omitempty"`
	MimeStructure  []mimebuild.Part      `json:"mime_structure,omitempty"`
}

// SendEmailOutput is the ses_send_email result.
type SendEmailOutput struct {
	MessageID      string                `json:"MessageId,omitempty"`
	WouldCall      *sesv2.SendEmailInput `json:"WouldCall,omitempty"`
	ServerMetadata ServerMetadata        `json:"ServerMetadata"`
}

// sendEmailOutputSchema is the inferred SendEmailOutput schema with Go []byte
// described as the base64 string encoding/json actually emits. Inference calls
// a []byte an array, so the SDK validated its own DryRun echo (WouldCall's
// Content.Raw.Data and Attachments[].RawContent) and failed the whole call —
// any DryRun carrying binary content came back to the client as a JSON-RPC
// error. Only this tool echoes []byte, so only this tool needs the override.
//
// The panic is also why ServerMetadata.MimeStructure is a flat []mimebuild.Part
// and not a tree: jsonschema.For errors on any named recursive type, so a
// Parts []Part field would panic here, inside NewServer, and take down every
// tool registration, cmd/gendocs, and the Lambda cold start for the sake of one
// diagnostic field. TestSendEmailOutputSchemaStaysInferable is the tripwire.
func sendEmailOutputSchema() *jsonschema.Schema {
	return outputSchemaFor[SendEmailOutput]("ses_send_email")
}

// outputSchemaFor infers a tool's output schema, correcting the two places
// where the inferred shape and encoding/json disagree about a WouldCall echo
// of an AWS SDK input struct. Inference describes Go's zero values by the Go
// type; encoding/json writes what the zero value actually becomes on the wire,
// and AddTool validates handler output against the inferred schema, so a
// mismatch fails the whole call with a JSON-RPC error rather than any
// diagnosable result:
//
//   - []byte infers as an array but marshals as a base64 string (every
//     ses_send_email DryRun carrying binary content failed this way from M2
//     until it was found);
//   - a nil map infers as a required, non-nullable object but marshals as
//     null (every sms_send_text_message DryRun failed this way on
//     SendTextMessageInput.DestinationCountryParameters, which the server
//     never sets).
//
// Both are properties of echoing SDK structs, so every tool with a WouldCall
// declares its schema through here rather than waiting to be the next one
// found. TestOutputSchemasStayInferable is the tripwire: the error path below
// panics inside NewServer, which would take down every tool registration,
// cmd/gendocs, and the Lambda cold start.
func outputSchemaFor[T any](tool string) *jsonschema.Schema {
	schema, err := jsonschema.For[T](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[[]byte]():            {Types: []string{"null", "string"}},
			reflect.TypeFor[map[string]string](): {Types: []string{"null", "object"}, AdditionalProperties: &jsonschema.Schema{Types: []string{"string"}}},
		},
	})
	if err != nil { // a type-shape error, not a runtime condition; AddTool panics on these too
		panic(tool + " output schema: " + err.Error())
	}
	return schema
}

// blockedResult turns the first blocking decision into the tool error, with the
// decisions so far attached so the model can explain the refusal (PRD 8). It is
// called twice: once before referenced attachments are fetched, once after.
func blockedResult(result guardrails.Result, dryRun bool) (*mcp.CallToolResult, bool) {
	blocked, isBlocked := result.Blocked()
	if !isBlocked {
		return nil, false
	}
	res := toolError("blocked by guardrail " + blocked.Name + ": " + blocked.Reason)
	res.StructuredContent = SendEmailOutput{
		ServerMetadata: ServerMetadata{Guardrails: result.Decisions, DryRun: dryRun},
	}
	return res, true
}

func (d Deps) sendEmail() mcp.ToolHandlerFor[schemas.SendEmailInput, SendEmailOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.SendEmailInput) (*mcp.CallToolResult, SendEmailOutput, error) {
		if res := requireScope(ctx, "msg/email:send"); res != nil {
			return res, SendEmailOutput{}, nil
		}
		s := d.Settings
		var result guardrails.Result

		// Shape rules first (PRD 5.3): exactly one of Simple / Raw, per
		// attachment exactly one of RawContent / RawContentKey, and for Raw
		// exactly one of Data / DataKey.
		if in.Content == nil || (in.Content.Simple == nil) == (in.Content.Raw == nil) {
			return toolError("Content must contain exactly one of Simple or Raw"), SendEmailOutput{}, nil
		}
		if in.Content.Simple != nil {
			for i, a := range in.Content.Simple.Attachments {
				if (a.RawContent == "") == (a.RawContentKey == "") {
					return toolError(fmt.Sprintf("attachment %d must set exactly one of RawContent or RawContentKey", i)), SendEmailOutput{}, nil
				}
			}
		}
		if in.Content.Raw != nil && (in.Content.Raw.Data == "") == (in.Content.Raw.DataKey == "") {
			return toolError("Content.Raw must set exactly one of Data or DataKey"), SendEmailOutput{}, nil
		}
		recipients := in.Destination.All()
		result.Add(guardrails.MaxRecipients(len(recipients), s.MaxRecipients))
		result.Add(guardrails.RecipientsAllowed(recipients, s.RecipientAllowList))
		// The guardrails decode the base64 payloads; the builder reuses the
		// bytes instead of decoding them a second time.
		var rawDecoded []byte
		var attDecoded [][]byte
		switch {
		case in.Content.Raw != nil && in.Content.Raw.Data != "":
			var decisions []guardrails.Decision
			rawDecoded, decisions = guardrails.RawEmail(in.Content.Raw.Data, s.EmailMaxRawBytes, s.SESSenderAddresses)
			for _, dec := range decisions {
				result.Add(dec)
			}
		case in.Content.Raw != nil:
			// Raw.DataKey has no rung to run yet: every one of them reads the
			// message, and the message is still in the files bucket.
		default:
			result.Add(guardrails.SenderAllowed(in.FromEmailAddress, s.SESSenderAddresses))
		}
		if d.Limiter != nil {
			result.Add(d.Limiter.Check(ctx, "ses_send_email"))
		}
		// Everything above is free to evaluate, so a refusal is decided before a
		// referenced attachment or a referenced raw message costs an S3 read: the
		// rate limiter is the cost control (PRD §8), and fetching first would let
		// a throttled caller keep reading the files bucket.
		//
		// Raw.DataKey is the one shape that cannot keep that property whole: the
		// From header sender_allow_list checks lives *inside* the object, so
		// that one decision necessarily runs after the read. Recipients, the
		// recipient count, and the rate limit still refuse before it, so the
		// read a disallowed sender can provoke is one HeadObject plus one
		// GetObject of an object already in the owner's own bucket.
		if res, blocked := blockedResult(result, in.DryRun); blocked {
			return res, SendEmailOutput{}, nil
		}
		if in.Content.Raw != nil && in.Content.Raw.DataKey != "" {
			if res := d.filesReadable(ctx, "Raw.DataKey", "Raw.Data"); res != nil {
				return res, SendEmailOutput{}, nil
			}
			body, res := d.fetchSharedObject(ctx, "Raw.DataKey", in.Content.Raw.DataKey)
			if res != nil {
				return res, SendEmailOutput{}, nil
			}
			// Identical ladder to an inlined Raw.Data from raw_size on, so a
			// message that would have been refused inline is refused by key too.
			rawDecoded = body
			for _, dec := range guardrails.RawEmailBytes(body, s.EmailMaxRawBytes, s.SESSenderAddresses) {
				result.Add(dec)
			}
		}
		if in.Content.Simple != nil {
			// From here a referenced attachment is indistinguishable from an
			// inline one: same size budget, same digest, same builder.
			resolved, res := d.resolveAttachments(ctx, in.Content.Simple.Attachments)
			if res != nil {
				return res, SendEmailOutput{}, nil
			}
			in.Content.Simple.Attachments = resolved
			if len(resolved) > 0 {
				atts := make([]guardrails.AttachmentInput, len(resolved))
				for i, a := range resolved {
					atts[i] = guardrails.AttachmentInput{FileName: a.FileName, RawContent: a.RawContent}
				}
				var decisions []guardrails.Decision
				attDecoded, decisions = guardrails.EmailAttachments(atts, s.EmailMaxRawBytes)
				for _, dec := range decisions {
					result.Add(dec)
				}
			}
		}
		if res, blocked := blockedResult(result, in.DryRun); blocked {
			return res, SendEmailOutput{}, nil
		}

		// An inline attachment gets a message this server assembles itself and
		// sends as Content.Raw: SES roots every Simple attachment under a
		// multipart/mixed, where a cid: reference never resolves, so the HTML
		// part and the image have to be siblings inside a multipart/related we
		// build (docs/plans/email-inline-mime.md). Every other send keeps SES's
		// own Simple assembly, so nothing that sends today changes shape.
		var assembled []byte
		var structure []mimebuild.Part
		if in.Content.Simple != nil {
			// Which attachments count as inline is InlineAttachments' rule and
			// not a second copy of it: it returns no decisions at all exactly
			// when nothing is inline, so an ordinary attachment send is still
			// evaluated the way it is today, and the checks can never cover a
			// different set of parts than the assembler places inline.
			inline := guardrails.InlineAttachments(
				inlineAttachments(in.Content.Simple.Attachments, attDecoded), htmlBody(in.Content.Simple))
			if len(inline) > 0 {
				for _, dec := range inline {
					result.Add(dec)
				}
				// Checked before assembling, so a refusal reaches the caller as
				// the guardrail that made it rather than as an opaque assembly
				// error.
				if res, blocked := blockedResult(result, in.DryRun); blocked {
					return res, SendEmailOutput{}, nil
				}
				var err error
				// The part list is Assemble's other return, reported as
				// ServerMetadata.mime_structure: the caller sees the tree it is
				// about to send without parsing the bytes back, which is the
				// diagnostic that was missing when the cid: images did not render.
				if assembled, structure, err = mimebuild.Assemble(assembleMessage(in, s.SESReplyTo, attDecoded)); err != nil {
					// Every Assemble failure is a caller mistake with no policy
					// to phrase it as: an address that will not parse, a
					// Content-ID that cannot go in a header, SEVEN_BIT on bytes
					// that are not 7-bit clean.
					return toolError("cannot assemble the inline message: " + err.Error()), SendEmailOutput{}, nil
				}
				result.Add(assembledSize(len(assembled)))
				if res, blocked := blockedResult(result, in.DryRun); blocked {
					return res, SendEmailOutput{}, nil
				}
			}
		}
		out := SendEmailOutput{ServerMetadata: ServerMetadata{Guardrails: result.Decisions, DryRun: in.DryRun}}

		// Digested after the guardrails pass and before the DryRun fork, so a
		// dry run and the real send report identical digests.
		var atts []schemas.Attachment
		if in.Content.Simple != nil {
			atts = in.Content.Simple.Attachments
		}
		out.ServerMetadata.ContentDigests = contentDigests(rawDecoded, atts, attDecoded, assembled)
		out.ServerMetadata.MimeStructure = mimeStructure(structure, rawDecoded)

		call := buildSendEmail(sendEmailParams{
			in:               in,
			configurationSet: s.SESConfigurationSet,
			defaultReplyTo:   s.SESReplyTo,
			rawDecoded:       rawDecoded,
			attDecoded:       attDecoded,
			assembled:        assembled,
		})
		if in.DryRun {
			out.WouldCall = call
			return nil, out, nil
		}
		resp, err := d.SES.SendEmail(ctx, call)
		if err != nil {
			res := toolError(awsclients.ErrorText(err))
			res.StructuredContent = out
			return res, SendEmailOutput{}, nil
		}
		out.MessageID = aws.ToString(resp.MessageId)
		return nil, out, nil
	}
}

// resolveAttachments replaces every RawContentKey with the object's bytes,
// base64-encoded into a copy of the attachment, and returns the attachments
// unchanged when none is referenced. Re-encoding costs one pass over bytes we
// already hold and keeps a single input shape for the guardrails, which is
// worth more than the saved allocation: referenced attachments then run the
// exact same decode, size, and digest path as inline ones.
func (d Deps) resolveAttachments(ctx context.Context, atts []schemas.Attachment) ([]schemas.Attachment, *mcp.CallToolResult) {
	referenced := false
	for _, a := range atts {
		referenced = referenced || a.RawContentKey != ""
	}
	if !referenced {
		return atts, nil
	}
	if res := d.filesReadable(ctx, "RawContentKey", "RawContent"); res != nil {
		return nil, res
	}
	resolved := make([]schemas.Attachment, len(atts))
	copy(resolved, atts)
	for i, a := range resolved {
		if a.RawContentKey == "" {
			continue
		}
		body, res := d.fetchSharedObject(ctx, fmt.Sprintf("attachment %d", i), a.RawContentKey)
		if res != nil {
			return nil, res
		}
		resolved[i].RawContent = base64.StdEncoding.EncodeToString(body)
	}
	return resolved, nil
}

// filesReadable gates both by-reference paths — an attachment's RawContentKey
// and a whole message's Raw.DataKey — on the files store existing at all and on
// the caller holding msg/read: reading the files bucket is a files-store read,
// so an email-only token must not be able to exfiltrate a shared object through
// an email. field and inline name the reference and the bytes-in-line
// alternative, so the refusal says what to do instead.
func (d Deps) filesReadable(ctx context.Context, field, inline string) *mcp.CallToolResult {
	if d.Files == nil {
		return toolError(field + " needs the files store, which this server is not configured with (no files_* tools); send the bytes inline as " + inline + " instead")
	}
	return requireScope(ctx, "msg/read")
}

// fetchSharedObject reads one shared object into memory. HeadObject comes first
// so an expired or oversize object is refused before its bytes are downloaded —
// the files bucket allows objects far larger than the email budget (PRD §8).
//
// label names the reference in every refusal ("attachment 0", "Raw.DataKey"):
// the two callers are far apart in the message, and an error that called a raw
// message "attachment 0" would send the caller looking at the wrong field.
func (d Deps) fetchSharedObject(ctx context.Context, label, key string) ([]byte, *mcp.CallToolResult) {
	if !strings.HasPrefix(key, "shared/") {
		return nil, toolError(label + ": Key must be under shared/")
	}
	s := d.Settings
	head, err := d.Files.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.FilesBucket), Key: aws.String(bucketPrefix + key),
	})
	if err != nil {
		if objectMissing(err) {
			return nil, toolError(fmt.Sprintf("%s: no object %s in the files bucket", label, key))
		}
		return nil, toolError(fmt.Sprintf("%s: reading %s failed: %s", label, key, awsclients.ErrorText(err)))
	}
	// Cleanup runs daily, so an object whose expiry has passed is still here;
	// attaching it would outlive the link the owner set. A missing or
	// unparseable stamp is left alone, exactly as CleanupFiles leaves it.
	if expires, parseErr := time.Parse(time.RFC3339, head.Metadata[expiresAtMetaKey]); parseErr == nil && !expires.After(time.Now()) {
		return nil, toolError(fmt.Sprintf("%s: the link for %s expired at %s and the object is awaiting cleanup; re-upload it to attach it",
			label, key, head.Metadata[expiresAtMetaKey]))
	}
	if size := aws.ToInt64(head.ContentLength); size > int64(s.EmailMaxRawBytes) {
		return nil, toolError(fmt.Sprintf("%s: %s is %d bytes, over the %d-byte email budget", label, key, size, s.EmailMaxRawBytes))
	}
	obj, err := d.Files.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.FilesBucket), Key: aws.String(bucketPrefix + key),
	})
	if err != nil {
		if objectMissing(err) {
			return nil, toolError(fmt.Sprintf("%s: %s disappeared between the size check and the read (expiry cleanup?); re-upload it", label, key))
		}
		return nil, toolError(fmt.Sprintf("%s: reading %s failed: %s", label, key, awsclients.ErrorText(err)))
	}
	defer func() { _ = obj.Body.Close() }()
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, toolError(fmt.Sprintf("%s: reading %s failed: %s", label, key, err))
	}
	return body, nil
}

// objectMissing reports the two shapes S3 uses for "not there": NotFound from
// HeadObject, NoSuchKey from GetObject.
func objectMissing(err error) bool {
	var notFound *s3types.NotFound
	var noSuchKey *s3types.NoSuchKey
	return errors.As(err, &notFound) || errors.As(err, &noSuchKey)
}

// contentDigests hashes the decoded binary parts of one send: the caller's own
// raw MIME message, or every attachment whose bytes decoded (attDecoded is
// index-aligned with atts, nil where a decode failed) plus the whole message
// when the server assembled one. Simple Subject/Body text is left out on
// purpose — it travels as plain JSON in the request, so digesting it would only
// add noise. Returns nil when nothing binary was sent.
//
// "raw" stays reserved for a caller-supplied Content.Raw, which is why the
// assembled message is a separate parameter with its own name: the early return
// below would otherwise replace every per-attachment digest with one for the
// whole message, and the per-attachment digests are how a caller proves its
// image survived (the two are never both set).
func contentDigests(rawDecoded []byte, atts []schemas.Attachment, attDecoded [][]byte, assembled []byte) []ContentDigest {
	if rawDecoded != nil {
		return []ContentDigest{digestOf("raw", rawDecoded)}
	}
	var digests []ContentDigest
	for i, a := range atts {
		if i >= len(attDecoded) || attDecoded[i] == nil {
			continue
		}
		// Indexed as well as named because two attachments may share a FileName.
		digests = append(digests, digestOf(fmt.Sprintf("attachment[%d]:%s", i, a.FileName), attDecoded[i]))
	}
	if assembled != nil {
		digests = append(digests, digestOf("assembled", assembled))
	}
	return digests
}

// mimeStructure describes the message that will actually leave: the part list
// Assemble already produced for a server-assembled message, or the same shape
// walked out of a caller-supplied Content.Raw (inline Data or a DataKey the
// server read), so a caller can debug a message it built itself. A Simple send
// has neither — SES assembles it and the server never sees the tree — so the
// field is absent there and omitempty keeps the metadata clean.
//
// A walk that fails does not fail the send. The bytes are untrusted input, the
// raw ladder has already checked everything that decides deliverability
// (raw_size, raw_mime, sender_allow_list), and refusing a send because a
// *diagnostic* could not be produced would be the tail wagging the dog. The
// caller sees no mime_structure, which is the same signal it gets for a Simple
// send.
func mimeStructure(assembled []mimebuild.Part, rawDecoded []byte) []mimebuild.Part {
	if assembled != nil {
		return assembled
	}
	if rawDecoded == nil {
		return nil
	}
	parts, err := mimebuild.Walk(rawDecoded, rawWalkMaxDepth, rawWalkMaxParts)
	if err != nil {
		return nil
	}
	return parts
}

func digestOf(part string, decoded []byte) ContentDigest {
	sum := sha256.Sum256(decoded)
	return ContentDigest{Part: part, Bytes: len(decoded), SHA256: hex.EncodeToString(sum[:])}
}

// inlineAttachments describes the resolved attachments to the inline
// guardrails, which own the FileName, ContentType, and ContentId rules SES used
// to enforce for us on the Simple path. decoded is index-aligned with atts.
func inlineAttachments(atts []schemas.Attachment, decoded [][]byte) []guardrails.InlineAttachment {
	out := make([]guardrails.InlineAttachment, len(atts))
	for i, a := range atts {
		out[i] = guardrails.InlineAttachment{
			FileName:         a.FileName,
			ContentType:      a.ContentType,
			Disposition:      a.ContentDisposition,
			ContentID:        a.ContentId,
			TransferEncoding: a.ContentTransferEncoding,
		}
		if i < len(decoded) {
			out[i].Bytes = len(decoded[i])
		}
	}
	return out
}

// htmlBody returns the message's HTML body, which is the only place a cid:
// reference can live.
func htmlBody(m *schemas.Message) string {
	if m.Body == nil || m.Body.HTML == nil {
		return ""
	}
	return m.Body.HTML.Data
}

// assembledSize meters the message the server built against SES's own ceiling.
//
// A Decision literal here rather than a function in internal/guardrails: that
// package carries a 100 %-per-function coverage gate that earns its keep on the
// string checks, and one length comparison is not worth triggering it.
func assembledSize(n int) guardrails.Decision {
	if n > sesMaxMessageBytes {
		return guardrails.Decision{Name: "assembled_size", Allowed: false,
			Reason: fmt.Sprintf("the assembled message is %d bytes, over SES's maximum of %d", n, sesMaxMessageBytes)}
	}
	return guardrails.Decision{Name: "assembled_size", Allowed: true,
		Reason: fmt.Sprintf("%d bytes assembled", n)}
}

// assembleMessage maps the tool input onto the assembler's input. attDecoded is
// index-aligned with the Simple attachments and carries the bytes the
// guardrails already decoded.
//
// Bcc is deliberately absent, and mimebuild.Message has no field for it: blind
// recipients ride on Destination.BccAddresses, which SES honours for a raw
// message without disclosing them, while a Bcc header inside the message would
// be delivered to everybody and leak every hidden recipient. replyTo is the
// effective Reply-To, which becomes a header here *instead of* the API
// parameter (see buildSendEmail). The Subject charset the caller may have
// declared has no counterpart: an unstructured header is emitted as RFC 2047
// encoded words in utf-8 whatever the body charsets are.
func assembleMessage(in schemas.SendEmailInput, defaultReplyTo string, attDecoded [][]byte) mimebuild.Message {
	simple := in.Content.Simple
	msg := mimebuild.Message{
		From:    in.FromEmailAddress,
		ReplyTo: replyToAddresses(in, defaultReplyTo),
		HTML:    htmlBody(simple),
		Date:    time.Now().UTC(),
	}
	if in.Destination != nil {
		msg.To = in.Destination.ToAddresses
		msg.Cc = in.Destination.CcAddresses
	}
	if simple.Subject != nil {
		msg.Subject = simple.Subject.Data
	}
	if simple.Body != nil {
		if simple.Body.Text != nil {
			msg.Text, msg.TextCharset = simple.Body.Text.Data, simple.Body.Text.Charset
		}
		if simple.Body.HTML != nil {
			msg.HTMLCharset = simple.Body.HTML.Charset
		}
	}
	for i, a := range simple.Attachments {
		att := mimebuild.Attachment{
			FileName:         a.FileName,
			ContentType:      a.ContentType,
			ContentID:        a.ContentId,
			Disposition:      a.ContentDisposition,
			TransferEncoding: a.ContentTransferEncoding,
			Description:      a.ContentDescription,
		}
		if i < len(attDecoded) {
			att.Content = attDecoded[i]
		}
		msg.Attachments = append(msg.Attachments, att)
	}
	return msg
}

// replyToAddresses is the effective Reply-To: the caller's, else the server's
// default (the sending domain hosts no mailboxes), else none. One function
// because the API parameter and the assembled message's header have to agree on
// it, and only one of the two ever carries it.
func replyToAddresses(in schemas.SendEmailInput, defaultReplyTo string) []string {
	switch {
	case len(in.ReplyToAddresses) > 0:
		return in.ReplyToAddresses
	case defaultReplyTo != "":
		return []string{defaultReplyTo}
	default:
		return nil
	}
}

// sendEmailParams is what buildSendEmail maps onto the SDK input. A struct
// rather than a parameter list because the list reached seven with the
// assembled path, four of them interchangeable []byte and string.
type sendEmailParams struct {
	in               schemas.SendEmailInput
	configurationSet string
	defaultReplyTo   string
	rawDecoded       []byte   // the caller's own Content.Raw, already decoded
	attDecoded       [][]byte // decoded Simple attachments, index-aligned
	assembled        []byte   // the message the server built, nil on the Simple path
}

// buildSendEmail maps the tool input onto the SDK input, injecting the
// server-owned fields (PRD 5.1 rule 2).
func buildSendEmail(p sendEmailParams) *sesv2.SendEmailInput {
	in := p.in
	call := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(in.FromEmailAddress),
		Content:          &sestypes.EmailContent{},
	}
	// The configuration set, the destination (BccAddresses included), and the
	// tags are API parameters rather than message content, so they are set the
	// same way whichever content shape follows.
	if p.configurationSet != "" {
		call.ConfigurationSetName = aws.String(p.configurationSet)
	}
	if in.Destination != nil {
		call.Destination = &sestypes.Destination{
			ToAddresses:  in.Destination.ToAddresses,
			CcAddresses:  in.Destination.CcAddresses,
			BccAddresses: in.Destination.BccAddresses,
		}
	}
	// A message we assembled carries its own Reply-To header, so the parameter
	// stays unset: SES would add a second Reply-To from it, and two of them
	// violate RFC 5322 §3.6 with clients disagreeing about which one wins. The
	// same effective value went into the header (see assembleMessage).
	if p.assembled == nil {
		call.ReplyToAddresses = replyToAddresses(in, p.defaultReplyTo)
	}
	for _, tag := range in.EmailTags {
		call.EmailTags = append(call.EmailTags, sestypes.MessageTag{
			Name: aws.String(tag.Name), Value: aws.String(tag.Value),
		})
	}
	if p.assembled != nil {
		call.Content.Raw = &sestypes.RawMessage{Data: p.assembled}
		return call
	}
	if in.Content.Raw != nil {
		call.Content.Raw = &sestypes.RawMessage{Data: p.rawDecoded}
		return call
	}
	simple := in.Content.Simple
	msg := &sestypes.Message{}
	if simple.Subject != nil {
		msg.Subject = content(simple.Subject)
	}
	if simple.Body != nil {
		msg.Body = &sestypes.Body{}
		if simple.Body.Text != nil {
			msg.Body.Text = content(simple.Body.Text)
		}
		if simple.Body.HTML != nil {
			msg.Body.Html = content(simple.Body.HTML)
		}
	}
	for i, a := range simple.Attachments {
		att := sestypes.Attachment{FileName: aws.String(a.FileName)}
		if i < len(p.attDecoded) {
			att.RawContent = p.attDecoded[i]
		}
		if a.ContentType != "" {
			att.ContentType = aws.String(a.ContentType)
		}
		if a.ContentDescription != "" {
			att.ContentDescription = aws.String(a.ContentDescription)
		}
		if a.ContentDisposition != "" {
			att.ContentDisposition = sestypes.AttachmentContentDisposition(a.ContentDisposition)
		}
		if a.ContentId != "" {
			att.ContentId = aws.String(a.ContentId)
		}
		if a.ContentTransferEncoding != "" {
			att.ContentTransferEncoding = sestypes.AttachmentContentTransferEncoding(a.ContentTransferEncoding)
		}
		msg.Attachments = append(msg.Attachments, att)
	}
	call.Content.Simple = msg
	return call
}

func content(c *schemas.Content) *sestypes.Content {
	out := &sestypes.Content{Data: aws.String(c.Data)}
	if c.Charset != "" {
		out.Charset = aws.String(c.Charset)
	}
	return out
}

// Identity is one entry in the ses_list_email_identities result.
type Identity struct {
	IdentityName   string `json:"IdentityName"`
	IdentityType   string `json:"IdentityType"`
	SendingEnabled bool   `json:"SendingEnabled"`
}

// ListIdentitiesOutput is the ses_list_email_identities result.
type ListIdentitiesOutput struct {
	EmailIdentities []Identity `json:"EmailIdentities"`
}

func (d Deps) listIdentities() mcp.ToolHandlerFor[schemas.ListEmailIdentitiesInput, ListIdentitiesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.ListEmailIdentitiesInput) (*mcp.CallToolResult, ListIdentitiesOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, ListIdentitiesOutput{}, nil
		}
		size := in.PageSize
		if size <= 0 || size > 100 {
			size = 25
		}
		resp, err := d.SES.ListEmailIdentities(ctx, &sesv2.ListEmailIdentitiesInput{PageSize: aws.Int32(size)})
		if err != nil {
			return toolError(awsclients.ErrorText(err)), ListIdentitiesOutput{}, nil
		}
		out := ListIdentitiesOutput{EmailIdentities: []Identity{}}
		for _, id := range resp.EmailIdentities {
			out.EmailIdentities = append(out.EmailIdentities, Identity{
				IdentityName:   aws.ToString(id.IdentityName),
				IdentityType:   string(id.IdentityType),
				SendingEnabled: id.SendingEnabled,
			})
		}
		return nil, out, nil
	}
}

// AccountOutput is the ses_get_account result.
type AccountOutput struct {
	ProductionAccessEnabled bool    `json:"ProductionAccessEnabled"`
	SendingEnabled          bool    `json:"SendingEnabled"`
	Max24HourSend           float64 `json:"Max24HourSend"`
	MaxSendRate             float64 `json:"MaxSendRate"`
	SentLast24Hours         float64 `json:"SentLast24Hours"`
}

func (d Deps) getAccount() mcp.ToolHandlerFor[schemas.GetAccountInput, AccountOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ schemas.GetAccountInput) (*mcp.CallToolResult, AccountOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, AccountOutput{}, nil
		}
		resp, err := d.SES.GetAccount(ctx, &sesv2.GetAccountInput{})
		if err != nil {
			return toolError(awsclients.ErrorText(err)), AccountOutput{}, nil
		}
		out := AccountOutput{
			ProductionAccessEnabled: resp.ProductionAccessEnabled,
			SendingEnabled:          resp.SendingEnabled,
		}
		if resp.SendQuota != nil {
			out.Max24HourSend = resp.SendQuota.Max24HourSend
			out.MaxSendRate = resp.SendQuota.MaxSendRate
			out.SentLast24Hours = resp.SendQuota.SentLast24Hours
		}
		return nil, out, nil
	}
}

// requireScope returns an isError result when the caller lacks scope.
func requireScope(ctx context.Context, scope string) *mcp.CallToolResult {
	principal, ok := httpapi.PrincipalFrom(ctx)
	if !ok {
		return toolError("no authenticated principal in context")
	}
	if err := auth.RequireScope(principal, scope); err != nil {
		return toolError(err.Error())
	}
	return nil
}
