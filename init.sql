CREATE TABLE IF NOT EXISTS trips (
    request_id UUID PRIMARY KEY,
    user_id VARCHAR(255) NOT NULL,
    source VARCHAR(255) NOT NULL,
    destination VARCHAR(255) NOT NULL,
    start_date DATE NOT NULL,
    end_date DATE NOT NULL,
    budget INTEGER,
    status VARCHAR(50) DEFAULT 'pending', -- Values: pending, completed, failed
    itinerary JSONB,                      -- Stores the full AI JSON response
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Create an index so looking up trips by user is fast
CREATE INDEX IF NOT EXISTS idx_user_id ON trips(user_id);