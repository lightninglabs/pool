# Channel Leases

## Overview

Once an order has been matched in an auction, the `pool auction leases` command can be used to examine your current set of purchased/sold channel leases. An example output looks something like the following:

```text
🏔 pool auction leases
{
        "leases": [
                {
                        "channel_point": "78cc6879c1dc1c00f22b29a06458f1335ed0fdb7d05c01b9077e3155e697bbb9:2",
                        "channel_amt_sat": 5000000,
                        "channel_duration_blocks": 144,
                        "premium_sat": 40000,
                        "execution_fee_sat": 5001,
                        "chain_fee_sat": 165,
                        "order_nonce": "eb972cd21cf1651e251c8b07d69e89c47294bae104fbdc9da26edf9aee335c9a",
                        "purchased": false
                }
        ],
        "total_amt_earned_sat": 40000,
        "total_amt_paid_sat": 5166
}
```

Here we can see I sold a channel for 40k satoshis, and ended up paying 5k satoshis in chain and execution fees, netting a cool 35k satoshi yield. Within the actual auction, these numbers will vary based on the chain fee rate, the market prices, and also the execution fees. Users can constraint how much chain fees they'll pay by setting the `--max_batch_fee_rate` argument when submitting orders.

## Service Level Lifetime Enforcement

In the alpha version of Pool, _script level enforcement_ isn't yet implemented. Script level enforcement would lock the maker's funds in the channel for the lease period. This ensures that they can't just collect the premium \(before coupon channels\) and close out the channel instantly. With script enforcement, they would be able to close the channel \(force close it\), but their funds would be unavailable until the maturity period has passed.

Instead, we've implemented a feature in `lnd` to prevent channels from being _cooperatively closed_ by the maker until the expiry height \(what we call the `thaw_height`\).

