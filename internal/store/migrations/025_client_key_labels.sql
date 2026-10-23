CREATE TABLE client_key_labels (
 key_id TEXT NOT NULL REFERENCES client_api_keys(key_id) ON DELETE CASCADE,
 label_id TEXT NOT NULL REFERENCES labels(label_id) ON DELETE RESTRICT,
 mode TEXT NOT NULL CHECK(mode IN('allow','block')),
 PRIMARY KEY(key_id,label_id)
) STRICT;
