CREATE TABLE platform_egress_exclusions (
    platform_id   TEXT    NOT NULL,
    egress_ip     TEXT    NOT NULL,
    action        TEXT    NOT NULL,
    status_code   INTEGER NOT NULL DEFAULT 0,
    node_hash     TEXT    NOT NULL DEFAULT '',
    created_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (platform_id, egress_ip)
);
