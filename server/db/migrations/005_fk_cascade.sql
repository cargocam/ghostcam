ALTER TABLE enrollment_tokens DROP CONSTRAINT IF EXISTS enrollment_tokens_claimed_by_fkey;
ALTER TABLE enrollment_tokens ADD CONSTRAINT enrollment_tokens_claimed_by_fkey FOREIGN KEY (claimed_by) REFERENCES cameras(device_id) ON DELETE SET NULL;
