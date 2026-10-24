-- TASK-054 (ADR-0026 companion): close the evasive-form coverage gaps the
-- TASK-047 adversarial corpus proved. Updates are unconditional by rule name
-- so reruns and upgrades converge to the same pattern (000020 convention).
-- Every changed pattern keeps a FINITE maximum match — derived by
-- packages/dlp/maxmatch.go, and the SSE holdback window re-derives from it
-- automatically:
--
--   openai_key       203 bytes (unchanged: only the prefix flag is scoped)
--   password_field   329 -> 332 bytes
--   internal_phone    11 ->  13 bytes
--
-- The rule-set maximum stays dominated by the unchanged db_conn_string
-- (~1039 bytes), so the streaming holdback window does not grow and
-- `make perf-gateway` must show no regression.

-- openai_key: accept the case-mangled prefix (SK- / Sk- / sK- — copy/paste
-- and formatter damage). The case-insensitivity is scoped to the prefix with
-- (?i:...) so the bounded [A-Za-z0-9]{20,200} body is byte-for-byte
-- unaffected.
-- False-positive review — benign text newly matched: any case-variant
-- "sk-" token followed by >= 20 alphanumerics (e.g. an uppercase ticket key
-- like "TASK-<20 alnum>"). The 20-character body requirement keeps such
-- collisions rare, the shape is overwhelmingly an API key, and the deny hit
-- is audited and reviewable; the same collision surface already existed for
-- lowercase input.
UPDATE dlp_rules SET pattern = '(?i:sk-)[A-Za-z0-9]{20,200}'
WHERE name = 'openai_key';

-- password_field: tolerate optional quoting around the key ("password" /
-- 'password' — the dominant JSON/API payload shape) and an optional opening
-- quote before the value. The value class excludes quote characters, so a
-- quoted value redacts to well-formed JSON: {"password": "[REDACTED:password]"}.
-- False-positive review — benign text newly matched: quoted or suffixed keys
-- ("db_password", "myPassword") followed by [:=] and a >= 4-character value.
-- Such keys are semantically password assignments, so redacting them is the
-- intended behavior, not collateral damage; the action is redact (rewrite,
-- never block), so a residual false hit costs one rewritten token, never a
-- blocked request. Empty/short values (< 4 chars) and placeholder "" stay
-- untouched.
UPDATE dlp_rules SET pattern = '(?i)["'']?(password|passwd|pwd)["'']?\s{0,32}[:=]\s{0,32}["'']?[^\s"'']{4,256}'
WHERE name = 'password_field';

-- internal_phone: accept the canonical CN 3-4-4 visual grouping
-- (138 0013 8000 / 138-0013-8000). Separators are allowed ONLY at the two
-- fixed digit-group boundaries and only as a single space or hyphen — never
-- a free-running digit-space class: runs split at arbitrary positions
-- ("1 3800138000") and 12+ digit runs still do not match, and the \b
-- boundary anchors are preserved.
-- False-positive review — benign text newly matched: by construction a match
-- is 11 digits starting with 1[3-9], i.e. a CN mobile number however it is
-- grouped; a benign collision would be a 3-4-4-grouped order/reference
-- number starting 13-19, which is rare, and the action is redact with the
-- tokenize vault available for reversible recovery.
UPDATE dlp_rules SET pattern = '\b1[3-9]\d[\s-]?\d{4}[\s-]?\d{4}\b'
WHERE name = 'internal_phone';

-- db_conn_string: DELIBERATELY UNCHANGED — accepted limitation (TASK-054).
-- The analysis: a password segment containing raw spaces escapes
-- [^\s"]{1,512}, but closing that gap inside the bounded-pattern policy is
-- not defensible:
--   1. Any space tolerance in the segment needs bounded repetition of the
--      form "word, optional space-run". No bound separates the evasive shape
--      from ordinary prose after a "scheme://user:" prefix — e.g. "see
--      postgres://db:5432 then ping alice@example.com" becomes a deny-class
--      hit and hard-blocks benign operations chat (422, zero egress).
--   2. The FUNCTIONAL representations of space-bearing passwords in URIs are
--      percent-encoded ("my%20s3cret"); '%' is inside the current
--      [^\s"] class, so working credential references are already covered.
--      A raw space is not valid URI userinfo (RFC 3986), so the escaping
--      form is a malformed string, not a usable connection string.
--   3. A bound generous enough for real passphrases (e.g. 8 space-separated
--      chunks of up to 512 bytes) would multiply the derived SSE holdback
--      (~1039 bytes) several times over for every stream, against ADR-0026's
--      streaming-liveness rationale.
-- The limitation is self-verifying in TestDLPRuleFindings
-- (db-conn-password-space): a future fix flips the subtest to positive
-- automatically. Also recorded in docs/verify/security-adversarial.md.
