-- TASK-039 (ADR-0026 companion): give every seeded DLP pattern a finite
-- maximum match length. The streaming sanitizer derives its holdback window
-- from the longest possible match; open-ended repetition (\s+, [^\s"]+,
-- {20,}) makes that derivation mathematically impossible and would force
-- either an unsafe guess or whole-response buffering. The new upper bounds
-- are far above any real credential's length (a 33-space gap inside
-- "bearer   x" or a 512-character password does not occur in real
-- traffic), so deny/redact behavior is unchanged for real content.
-- Updates are unconditional by rule name: reruns converge to the same
-- bounded pattern.

UPDATE dlp_rules SET pattern = 'sk-[A-Za-z0-9]{20,200}'
WHERE name = 'openai_key';

UPDATE dlp_rules SET pattern = 'agk_(live|test)_[A-Za-z0-9]{16,200}'
WHERE name = 'eap_api_key';

UPDATE dlp_rules SET pattern = '(?i)bearer\s{1,32}[A-Za-z0-9\-._~+/]{20,1000}'
WHERE name = 'bearer_token';

UPDATE dlp_rules SET pattern = '(?i)(password|passwd|pwd)\s{0,32}[:=]\s{0,32}\S{4,256}'
WHERE name = 'password_field';

UPDATE dlp_rules SET pattern = '(?s)-----BEGIN [A-Z ]{0,64}PRIVATE KEY-----'
WHERE name = 'private_key';

UPDATE dlp_rules SET pattern = '(?i)(postgres|postgresql|mysql|mongodb)://[^\s"]{1,512}:[^\s"]{1,512}@'
WHERE name = 'db_conn_string';
