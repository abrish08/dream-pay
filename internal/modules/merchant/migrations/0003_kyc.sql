-- KYC: a self-signed-up merchant goes live only after its verification is approved.
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS kyc_status TEXT NOT NULL DEFAULT 'not_started';
-- Merchants already active before KYC existed were verified offline.
UPDATE merchants SET kyc_status = 'waived' WHERE status <> 'pending' AND kyc_status = 'not_started';
