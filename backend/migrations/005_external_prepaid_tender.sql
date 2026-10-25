-- Tender for money that already moved on another system before the order
-- reached BeepBite.
--
-- Vers & Belegd (and any similar hosted checkout) takes payment itself —
-- Mollie on that site, or a house-account invoice later. Recording the same
-- order here as cash or card would claim this till took the money. The row
-- is offline on purpose: this process never calls a payment provider.
-- requires_reference is true so a Mollie payment id, or the other system's
-- order id, is what a person reconciles against.

INSERT INTO payment_methods (code, name, kind, is_active, requires_reference, supports_tips)
VALUES (
    'external_prepaid',
    'Paid on another channel',
    'offline',
    true,
    true,
    false
)
ON CONFLICT (code) DO UPDATE
    SET name               = EXCLUDED.name,
        kind               = EXCLUDED.kind,
        is_active          = EXCLUDED.is_active,
        requires_reference = EXCLUDED.requires_reference,
        supports_tips      = EXCLUDED.supports_tips;
