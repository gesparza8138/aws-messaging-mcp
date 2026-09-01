package mimebuild

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
)

// Walk parses an assembled message and describes its parts in the same flat
// shape Assemble returns, so the tree a caller is about to send is observable
// without re-reading the bytes by hand.
//
// The bytes may be the caller's own (a hand-built Content.Raw), so the walk is
// bounded: parts deeper than maxDepth are not descended into, and the list
// stops at maxParts. Both stop cleanly and return what was found - a 10 MB
// message can hold millions of empty parts, and refusing to describe it is
// less useful than describing the first maxParts of it. Anything malformed is
// an error, never a panic.
//
// Bytes is the part's body as it appears in the message, still transfer
// encoded; a multipart container reports 0 and its children carry the bytes.
func Walk(msg []byte, maxDepth, maxParts int) ([]Part, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		return nil, fmt.Errorf("cannot parse the message headers: %w", err)
	}
	var parts []Part
	if err := walkPart(textproto.MIMEHeader(parsed.Header), parsed.Body, "1", 0, maxDepth, maxParts, &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// walkPart appends one part and, when it is a container, its children. It uses
// NextRawPart rather than NextPart: NextPart transparently decodes a
// quoted-printable body and hides the Content-Transfer-Encoding header, which
// would make the byte counts disagree with the ones Assemble reports for the
// very same message.
func walkPart(header textproto.MIMEHeader, body io.Reader, path string, depth, maxDepth, maxParts int, parts *[]Part) error {
	if len(*parts) >= maxParts {
		return nil
	}
	mediaType, params, err := partMediaType(header, path)
	if err != nil {
		return err
	}
	part := Part{
		Path:        path,
		Depth:       depth,
		ContentType: mediaType,
		ContentID:   bareContentID(header.Get("Content-Id")),
	}
	part.Disposition, part.FileName = disposition(header)
	if part.FileName == "" {
		part.FileName = params["name"]
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		n, err := io.Copy(io.Discard, body)
		if err != nil {
			return fmt.Errorf("part %s: cannot read the body: %w", path, err)
		}
		part.Bytes = int(n)
		*parts = append(*parts, part)
		return nil
	}
	*parts = append(*parts, part)
	if depth >= maxDepth {
		return nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("part %s: %s has no boundary parameter", path, mediaType)
	}
	reader := multipart.NewReader(body, boundary)
	for i := 1; ; i++ {
		if len(*parts) >= maxParts {
			return nil
		}
		child, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("part %s: %w", path, err)
		}
		if err := walkPart(child.Header, child, path+"."+strconv.Itoa(i), depth+1, maxDepth, maxParts, parts); err != nil {
			return err
		}
	}
}

// partMediaType returns the part's media type and parameters. A part with no
// Content-Type at all is text/plain, which is the RFC 2045 default and common
// enough in messages we did not write; a Content-Type that will not parse is
// an error, because the walk cannot descend without its boundary.
func partMediaType(header textproto.MIMEHeader, path string) (string, map[string]string, error) {
	value := header.Get("Content-Type")
	if value == "" {
		return "text/plain", map[string]string{}, nil
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", nil, fmt.Errorf("part %s: cannot parse Content-Type %q: %w", path, value, err)
	}
	return mediaType, params, nil
}

// disposition returns the part's disposition and filename. Unlike the content
// type this one is only descriptive, so a header that will not parse is
// reported as no disposition rather than failing the whole walk.
func disposition(header textproto.MIMEHeader) (string, string) {
	value := header.Get("Content-Disposition")
	if value == "" {
		return "", ""
	}
	parsed, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", ""
	}
	return parsed, params["filename"]
}

// bareContentID strips the angle brackets a Content-ID header carries, so the
// value matches the cid: form the HTML references and the form Assemble
// reports.
func bareContentID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1 && strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
		return value[1 : len(value)-1]
	}
	return value
}
