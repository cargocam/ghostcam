-- Drop the audit_log table: nothing ever wrote to it (the server used
-- log/slog for audit events and never called InsertAuditEntry). The
-- corresponding admin query endpoint has also been removed.
DROP TABLE IF EXISTS audit_log;
