ALTER TABLE users ADD COLUMN error_notify INTEGER NOT NULL DEFAULT 0;
INSERT INTO users (user_id, platform, error_notify) VALUES (282044798, 'tg', 1)
    ON CONFLICT(user_id) DO UPDATE SET error_notify = 1;
