ALTER TABLE users ADD COLUMN error_notify BOOLEAN NOT NULL DEFAULT FALSE;
INSERT INTO users (user_id, platform, error_notify) VALUES (282044798, 'tg', TRUE)
    ON CONFLICT (user_id) DO UPDATE SET error_notify = TRUE;
