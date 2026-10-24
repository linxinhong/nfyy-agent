package main

// Attachment upload (PUT /enterprise/v1/blobs/{sha256}): members upload the
// bytes a contribution will cite. Registered only when EAP_BLOB_ROOT is set.
//
// Pitfalls:
//   - Text attachments are read whole (at most maxTextBlob) and DLP-scanned
//     before anything is written; any hit refuses the upload, like a
//     contribution body. Binary types are type-checked only.
//   - The frozen error codes have no 413: oversize is invalid_argument.

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"enterprise-agent-platform/packages/blob"
	"enterprise-agent-platform/packages/errs"
)

const maxTextBlob = 5 << 20

func blobProblem(err error) error {
	switch {
	case errors.Is(err, blob.ErrInvalid):
		return errs.New(errs.InvalidArgument, "path must be a lowercase hex sha256 and the body non-empty")
	case errors.Is(err, blob.ErrHashMismatch):
		return errs.New(errs.InvalidArgument, "uploaded bytes do not match the sha256 in the path")
	case errors.Is(err, blob.ErrTooLarge):
		return errs.New(errs.InvalidArgument, "attachments are at most 50 MiB (text 5 MiB)")
	case errors.Is(err, blob.ErrMediaType):
		return errs.New(errs.InvalidArgument, "Content-Type is not an allowed document or image type, or does not match the bytes")
	case errors.Is(err, blob.ErrBlocked):
		return errs.New(errs.InvalidArgument, "attachment blocked by the malware scan")
	case errors.Is(err, blob.ErrScan):
		return errs.Wrap(err, errs.UpstreamUnavailable, "malware scan unavailable; retry later").AsRetriable()
	default:
		return errs.Wrap(err, errs.Internal, "attachment storage failed")
	}
}

// handlePutBlob implements PUT /enterprise/v1/blobs/{sha256}.
func (s *Server) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	session, author, r, ok := s.contributionAuthor(w, r)
	if !ok {
		return
	}
	mediaType := blob.NormalizeMediaType(r.Header.Get("Content-Type"))
	var body io.Reader = http.MaxBytesReader(w, r.Body, blob.MaxSize+1)
	if blob.IsText(mediaType) {
		raw, err := io.ReadAll(io.LimitReader(body, maxTextBlob+1))
		if err != nil {
			writeProblem(w, r, errs.New(errs.InvalidArgument, "could not read the attachment body"))
			return
		}
		if len(raw) > maxTextBlob {
			writeProblem(w, r, blobProblem(blob.ErrTooLarge))
			return
		}
		if s.DLP == nil {
			writeProblem(w, r, errs.DLPFailClosed("attachment DLP engine is not configured"))
			return
		}
		scan, err := s.DLP.Scan(r.Context(), string(raw))
		if err != nil {
			writeProblem(w, r, errs.DLPFailClosed(err.Error()))
			return
		}
		if len(scan.RuleNames) > 0 {
			writeProblem(w, r, errs.New(errs.DLPBlocked, "attachment contains sensitive content; redact it locally first"))
			return
		}
		body = bytes.NewReader(raw)
	}
	stored, created, err := s.Blobs.Put(r.Context(), session.Workspace.WorkspaceID, author.UserID, r.PathValue("sha256"), mediaType, body, s.contributionEvent(r, session))
	if err != nil {
		writeProblem(w, r, blobProblem(err))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, stored)
}
