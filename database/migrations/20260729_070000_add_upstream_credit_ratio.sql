-- Allow upstreams to declare a credit bonus ratio so routing compares paid value.
ALTER TABLE upstreams
    ADD COLUMN credit_ratio DOUBLE PRECISION NOT NULL DEFAULT 1;

ALTER TABLE upstreams
    ADD CONSTRAINT upstreams_credit_ratio_positive
    CHECK (credit_ratio > 0);
