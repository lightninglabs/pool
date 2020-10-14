# Batch Execution

## Batched Uniform-Price Clearing

Now that we have orders submitted, how does the rest of the auction actually work? As mentioned above, Pool conducts a _discrete_ batch auction every 10 minutes. This is distinct from regular continuous exchanges in that orders are only cleared every 10 minutes. Orders are also sealed-bid, meaning that other traders in the venue are unable to see what others have bid. On top of this, we utilize a uniform-clearing price algorithm to give all traders in the batch the _same_ interest rate. This is the same mechanism used by the U.S Treasury for its bonds, and is intended to promote fairness as your order will only be matched with a price better than your initial ask/bid.

Note that it's possible that after the 10 minutes interval has passed a market can't be made \(supply and demand didn't cross\). In this case, nothing happens, and we just wait for the next batch to come across.

To illustrate how the uniform price clearing works consider the following example. Let's say I want to buy 100 million satoshis \(1 BTC, 1000 units\), for at least 10 days \(1440 blocks\) at a price of 5% \(using high numbers to make it easy to follow\). However, the _market clearing price_ \(where the supply+demand curves cross\) is actually 1%. In this case I bid _more_ than the market clearing price, but end up paying that price, as it's the best price that was possible in that market.

A simple rule of thumb for bids and asks is as follows:

* When I submit a bid, I'll either pay that amount or less.
* When I submit an ask, I'll either receive that amount or more.

All orders in a batch are executed in a _single_ on-chain transaction. This allows for thousands of channels to be bought/sold atomically in a single block. We call the transaction that executes the orders the Batch Execution Transaction.

The `pool auction` sub-command houses a number of useful commands to explore the past batches, and examine the current auction parameters.

One can browse the latest cleared batch using the `pool auction snapshot` command:

```text
🏔 pool auction snapshot 
{
        "version": 0,
        "batch_id": "02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2ae66afefb8eaf27f",
        "prev_batch_id": "03687baa3c7414e800ddba37edacb3281999739303b7290a69bd457f428ecd9b2c",
        "clearing_price_rate": 976,
        "matched_orders": [
                {
                        "ask": {
                                "version": 0,
                                "max_duration_blocks": 4032,
                                "rate_fixed": 744,
                                "chan_type": 0
                        },
                        "bid": {
                                "version": 0,
                                "min_duration_blocks": 1024,
                                "rate_fixed": 976,
                                "chan_type": 0
                        },
                        "matching_rate": 976,
                        "total_sats_cleared": "1000000",
                        "units_matched": 10
                }
        ],
        "batch_tx_id": "4508169e371096ad85e57f251e7b0034910a5e4799f3e9714d7df98f85fd8b93",
        "batch_tx": "0200000000010307368f8721608c58743c452562b4fb300f3a983e0ce32e16975236493de64b4a01000000000000000019947c40c19c14f3e0ba9795c80e878e5ac9d19513f95fb63204590603c78e7a000000000000000000c6bbf036ec29cc79efa15e4b779ae33286ddf3d0ff06fba720b58e9652f030bb010000000000
0000000440420f0000000000220020169c54346374ed74d0654d4fc6fa493c637cdd8ce7c76ad24a476e7d370b926697490f0000000000220020c13828d72d6a3fd12e939d46153ebcb2cc1c7bbb0958d53d92701ba1ba5930eb0bbbc901000000002200201ec50230e41f0f0978e1b0c475bfe8af1e032135b65233a86affd9d56b320f6e99adeb020
000000022002026e0d02777ed45059d70233dfdec0aa30abe40fdc26fad5aa780f9448a399118024730440220641ad6ef4d754ad7e6164c9743b549d194db1b0a1d4fd1c4c8f47b6e044203e402206db731b2b0eebd9244f27118aaeb85bd7679769eac57cba395837a3c8b4ff24101232103ba06cff976b410f9381f297d9693544a19c504527f5a4c
c0eb2966b3900343b6ac0347304402207b0344aa98878e5aa40dc0fb712beff9b11d7fba3671f847996d83b4f6a643f90220720aa47f0ac229e4d14eebe38cffbdb2a344241e56151a0ce057f9c4cc001a1201483045022100be8808e71b6867521ed16c7749d78fb809ea0fc72f33d3b2b752cb4a13bc4ad802202335fda42f030a058003437dc7e05
39a6d36f3ce94045e20f3e176f165bc5ef0014e2103d9ebf3cea856f88ee98801621b7ea837951c530f69bc26da94d58f13417a4993ad2103a6051079a5910dd7c8d055b6713bdc0370e4983ee048a7ae26d9c52f7321949fac7364038b341bb1680347304402202ed63c0225afc718169c081b33e1bb2049cee8126539275ad62afcdf17adf74a0220
04d6c18d9a98e60642e4665428cccd71ebbc2e30fe81aee5b2bf10d682875dc901483045022100ba598f8480ed6dcdbdda1e30166b43a86f72bbc23d20bfe8751553bc8ecc6a3f02203412399095fd1429b924bfe224b64f0840172686a8af9dd3b18dc4ed40de1e23014e21038be01624676bf63a9d7d829175a70193a7e8680452b9b192ec6cf6654
a7e3be1ad2103bc6202b694e62a4d890cbb83f3a4dddb964fc500b25f55a38501642a770e3f37ac7364038a341bb16800000000"
```

Here we see a batch where a single order was matched, at a clearing rate of `976`, with a single channel being purchased with a lifetime of `1024` blocks, or roughly one week.

Note that the `pool auction snapshot` command can be used to determine the past marker clearing price, which can be useful when deciding what your bid/ask should be. There's no explicit "market buy" function, but submitting a bid/ask at a similar `clearing_price_rate` is equivalent.

The command also accept a target `batch_id` as well. Here we can use the `prev_batch_id` to examine the _prior_ batch, similar to traversing a link-listed/blockchain:

```text
🏔 pool auction snapshot --batch_id=03687baa3c7414e800ddba37edacb3281999739303b7290a69bd457f428ecd9b2c
```

