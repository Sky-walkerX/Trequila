CREATE TABLE IF NOT EXISTS trips (
    request_id TEXT PRIMARY KEY,
    user_id VARCHAR(255) NOT NULL,
    source VARCHAR(255) NOT NULL,
    destination VARCHAR(255) NOT NULL,
    start_date DATE NOT NULL,
    end_date DATE NOT NULL,
    budget INTEGER,
    status VARCHAR(50) DEFAULT 'pending',
    itinerary JSONB,
    images JSONB,
    places JSONB,  -- <--- Added this column
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_user_id ON trips(user_id);