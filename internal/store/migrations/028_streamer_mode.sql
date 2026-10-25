ALTER TABLE client_api_keys ADD COLUMN streamer_mode INTEGER NOT NULL DEFAULT 0 CHECK (streamer_mode IN (0,1));
