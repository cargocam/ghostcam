-- Move public_key from camera_public_keys into cameras table directly.
-- Also drops the unused camera_api_keys table (Bearer auth removed).

ALTER TABLE cameras ADD COLUMN IF NOT EXISTS public_key TEXT;

-- Migrate existing public keys.
UPDATE cameras c
SET public_key = cpk.public_key
FROM camera_public_keys cpk
WHERE c.device_id = cpk.device_id;

DROP TABLE IF EXISTS camera_public_keys;
DROP TABLE IF EXISTS camera_api_keys;
