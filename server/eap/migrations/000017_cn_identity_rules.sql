-- TASK-030: checksum-validated Chinese identity rules (GB 11643 / Luhn /
-- GB 32100). Validators are attached by rule name in code; the regex is the
-- candidate filter and the checksum decides.

INSERT INTO dlp_rules (name, pattern, action, replacement) VALUES
    ('cn_resident_id', '\b[1-9]\d{5}(19|20)\d{2}(0[1-9]|1[0-2])(0[1-9]|[12]\d|3[01])\d{3}[0-9Xx]\b', 'redact', '[REDACTED:resident_id]'),
    ('cn_bank_card', '\b[3-9]\d{12,18}\b', 'redact', '[REDACTED:bank_card]'),
    ('cn_uscc', '\b[159][123]\d{6}[0-9A-Z]{10}\b', 'redact', '[REDACTED:uscc]')
ON CONFLICT (name) DO NOTHING;
