# Batch Execution

Now that we have orders submitted, how does the rest of the auction actually work? As mentioned above, Pool conducts a _discrete_ batch auction every 10 minutes. This is distinct from regular continuous exchanges in that orders are only cleared every 10 minutes. Orders are also sealed-bid, meaning that other traders in the venue are unable to see what others have bid. On top of this, we utilize a uniform-clearing price algorithm to give all traders in the batch the _same_ interest rate. This is the same mechanism used by the U.S Treasury for its bonds, and is intended to promote fairness as your order will only be matched with a price better than your initial ask/bid.

Note that it's possible that after the 10 minutes interval has passed a market can't be made \(supply and demand didn't cross\). In this case, nothing happens, and we just wait for the next batch to come across.

### Matchmaking

The first stage in batch execution is the matchmaking that is done by the auctioneer. Before actual matchmaking is performed, the auctioneer looks at the current fee climate and decides what fee rate should be used for the final Batch Execution Transaction, in order for the transaction to confirm in a timely manner. All orders that have their `max_batch_fee_rate` set to a value lower than the chosen fee rate are ignored from the auction this time around, but will be reconsidered if the fee climate changes before the next auction. This allows traders with a low time preference to submit orders that will stay around on the order book and only be considered for matchmaking in times of low chain fees.

Now that the transaction fee rate has been chosen, all orders still on the order book are considered, and the auctioneer matches asks with bids that pay at least the desired rate. The unsigned Batch Execution Transaction \(BET\) is assembled and presented to all traders that had their orders matched, along with information about the node they matched with.

### Batch Signing

When the trader's client receives a notification about a batch that is under execution, it checks that every part of the BET that affects the trader meets its requirements. This includes checking that the premium paid to the maker matches the batch clearing price, and that the chain fee deducted from the trader's account doesn't violate the max fee rate the trader agreed to.

Each match the trader was part of results in a channel output on the BET, and the trader will also ensure this is well formed, and of the expected amount. When that has been verified, the trader connects to the node on the other side of the match, and starts the channel opening process with the channel peer. Technically this is done by setting up a _funding shim_ with the backing `lnd` node, which prepares `lnd` for a funding transaction setting up a channel with the given parameters to be broadcast.

Only when all these checks are satisfactory and the channel funding shim has been successfully set up, the trader signs its input to the batch transaction and responds to the auctioneer. This ensures the trader is always _fully in custody of its own funds_, and never signs a transaction that would send the funds to an output it doesn't control.

Note that the trader can reject signing the batch for any reason, even when the BET is well formed. For instance, connecting to the channel peer can fail, resulting in the channel not being ready to be funded. The trader will reject this match, and matchmaking can start over, making sure the trader won't be matched with this channel peer again.

### Batch Publication

When all participating traders have signed their inputs in the Batch Execution Transaction, the auctioneer can sign the final input and broadcast the transaction. This transaction can be large, and serve as the funding transaction for potentially hundres of channels! The participating traders only pay chain fees for their inputs and outputs in the transaction, so everybody is saving substantially on fees compared to individually funding channels. If the trader supports account autorenewal and the account was close to expire, its expiry height will be automatically extended after the batch is sucessfully executed.

## Batched Uniform-Price Clearing

To illustrate how the uniform price clearing works consider the following example. Let's say I want to buy 100 million satoshis \(1 BTC, 1000 units\), for 2 weeks \(2016 blocks\) at a price of 5% \(using high numbers to make it easy to follow\). However, the _market clearing price_ \(where the supply+demand curves cross\) is actually 1%. In this case I bid _more_ than the market clearing price, but end up paying that price, as it's the best price that was possible in that market.

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
        "batch_id": "02a25623c6ebb497758d7f0bfbe8b06785f3e14364b4da689be5fe0f2dfbaec4ba",
        "prev_batch_id": "03aeeff15f081c61326914bc7026787dbc16e5734a3667af52cca6ddc419f583e6",
        "clearing_price_rate": 1636,
        "matched_orders": [
                {
                        "ask": {
                                "version": 0,
                                "lease_duration_blocks": 2016,
                                "rate_fixed": 1488,
                                "chan_type": 0
                        },
                        "bid": {
                                "version": 0,
                                "lease_duration_blocks": 2016,
                                "rate_fixed": 1636,
                                "chan_type": 0
                        },
                        "matching_rate": 1636,
                        "total_sats_cleared": "2500000",
                        "units_matched": 25
                }
        ],
        "batch_tx_id": "ae8c78f6b66747d5e94a533dd067fcd939e637e429079a7e19d7542993dc8922",
        "batch_tx": "02000000000103970968b4ebcc4255aa6259550482c5aeaa721a8351eb74b24d9542b3e33cafb0030000000000000000970968b4ebcc4255aa6259550482c5aeaa721a8351eb74b24d9542b3e33cafb0050000000000000000972f651388d2795cd4e55245f5651d53f839aa3fb4da384d911ab9e7940030de00000000000000000004cef60e0000000000220020034a9f33d0cc93882ad661e5f2de1a325b1fb60cb74c231def608ce057e9fb4ca025260000000000220020293a5728f0cd10a5fff3d1c9141faeb7c800c5488a5f02855abe84b62eb89cdf1e85a000000000002200200b6b0b965fea009f90886a970abbf3b73ba9a4bb1d0e5b5f7ec4f476fc426842394bd700000000002200209e6aeceeaa38fe15b19d8461eecbadfffc4912e628d5298a43dcf246856a9d0c02483045022100fc47d841497421d1cf400a4f9c3dfee5656fe430f2261cc2f77adc5301f4789a022041640b283982a5bf1ac481e71bfd9f429283196c5879780cbd26bf912371ed080123210336802cf05c208ff14ab7087ff4f044094d530abcb0b9be4b4f27532e99f4818cac03483045022100c9ea0c8250a4a697462368c2535623ebc3eb0cd3c1a956d4591830225adf109d0220115ab2c1f05217aae62aef3aa9c0aec0890a71b181cb0668e1029c12d7856a6b01483045022100f39d1fa072eeaeff1b19ee964d5b525acb39213180eb954762eeea80606403d1022058c0641b96047a6856d1c1a6e33f6ccea3ecc2a6a6b438c871f471a841816734014e2103513e45fa52a6c9d2a3a123f6daa909466cec87c7c76374e2a297e0a2b613d456ad21022a84f6765208b78d1f239c39bfdd44666765e252a89a66b3db147e842e5783d1ac7364033c270ab16803473044022039995bb6c1bae2264ae3f2bda3cec663e479e93be87a4e1b0b84ea4becc6edee022060524a39d56e14df327e5df90680d8cd08503a442d1ae135f58b34c95f1332c50147304402201bb5fc15c23f31e8b71586391e21af47ed2fe23f1ae2b866958212fbbacc6760022070d188ba480ec5cc736d6499a0ecb05b415b9ec77ca86da26a6f6cf8b2be64db014e21036f2559c0c914c413c730bc009a800f6941d02b68a3f05cef546bb747d5ad8352ad2103d6c97bb0ae68bffa2bfc09d7e06051dcfba637f3716691385021cd877eb3933eac73640320120ab16800000000"
}
```

Here we see a batch where a single order was matched, at a clearing rate of `1636`, with a single channel being purchased with a lifetime of `2016` blocks, or roughly two weeks.

Note that the `pool auction snapshot` command can be used to determine the past marker clearing price, which can be useful when deciding what your bid/ask should be. There's no explicit "market buy" function, but submitting a bid/ask at a similar `clearing_price_rate` to the historical one should put you close to where the demand in the market is.

The command also accept a target `batch_id` as well. Here we can use the `prev_batch_id` to examine the _prior_ batch, similar to traversing a link-listed/blockchain:

```text
🏔 pool auction snapshot --batch_id=03687baa3c7414e800ddba37edacb3281999739303b7290a69bd457f428ecd9b2c
```

## Fees

There are various fees paid and accumulated during a successful batch execution. The easiest way to get an overview of the total impact of fees on ones account is to run

```text
🏔 pool auction leases
{
        "leases": [
                {
                        "channel_point": "82c74922552670868e445541e7be5d31b3d27a020a004ea2fbab78cf39d0248b:0",
                        "channel_amt_sat": 1200000,
                        "channel_duration_blocks": 2016,
                        "channel_lease_expiry": 1864927,
                        "channel_node_key": "02ae4b61245c0baafaf273c3420bd2fbd289f3b81912d570367e15bf404ad410de",
                        "channel_node_tier": "TIER_1",
                        "premium_sat": 59998,
                        "clearing_rate_price": 24801,
                        "order_fixed_rate": 4960,
                        "execution_fee_sat": 1201,
                        "chain_fee_sat": 165,
                        "order_nonce": "8f85509e500d5978120729cb43e4a6db7de79c9548d92bac93c984cbf93c8dac",
                        "purchased": false
                },
                ...
                <removed>
                ...
                {
                        "channel_point": "9f8142e50e9fac09a6a3fa2487b2b5dcc0a3a6bf2385adc6475492a3f4c5e1a4:2",
                        "channel_amt_sat": 1200000,
                        "channel_duration_blocks": 2016,
                        "channel_lease_expiry": 1864948,
                        "channel_node_key": "021c97a90a411ff2b10dc2a8e32de2f29d2fa49d41bfbb52bd416e460db0747d0d",
                        "channel_node_tier": "TIER_1",
                        "premium_sat": 11999,
                        "clearing_rate_price": 4960,
                        "order_fixed_rate": 4960,
                        "execution_fee_sat": 1201,
                        "chain_fee_sat": 165,
                        "order_nonce": "63ebb5c69c0e2fa4907b034663c8697d5ed1255d29221106ae413cc015349d6a",
                        "purchased": true
                }
        ],
        "total_amt_earned_sat": 59998,
        "total_amt_paid_sat": 75037
}
```

Here you can see a summary of all matches you've been part of and that resulted in a channel being opened. In the first example the trader sold a 1,200,000 sat channel and got paid a premium of 59,998 sats for this liquidity. To be part of this batch, the trader paid an execution fee of 1201 sats, and a chain fee of 165 sats, netting 59998-1201-165 = 58632 sats that got paid to its account.

In the last example in the list above, the trader this time around bought a channel of 1,200,000 sats, and paid a premium of 11999 to the seller. Also this time around the trader paid an execution fee and chain fee, resulting in a total cost of 11999+1201+165 = 13365 sats.

Finally, a total tally of fees earned and paid is given. The command takes optional account and batch id arguments in case you want to filter the leases returned.

