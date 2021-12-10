# Account Recovery

In this section, we'll cover the possible ways of recovering the funds within a
Pool account upon data corruption/loss. Account funds are locked to a 2-of-2
multi-sig output of the account owner and the Pool auctioneer until the
account's expiration has been met, giving the account owner the security that
funds will not be spend without the owner's consent. This expiration is included
in the account output script, so it must be known in order to spend the account
funds. There are two possible ways to recover an account's funds after data 
corruption/loss:

- With Pool auctioneer's cooperation: which is the only method currently 
supported.
- Without Pool auctioneer's cooperation: which will require storage of an 
additional data blob similar to the Static Channel Backups present within 
`lnd`. 

An auctioneer-assisted account recovery intent can be issued through the 
`pool accounts recover` command or the `RecoverAccounts` RPC.

```text
$ pool accounts recover -h
NAME:
   pool accounts recover - recover accounts after data loss with the help of the auctioneer

USAGE:
   pool accounts recover [arguments...]

DESCRIPTION:

  In case the data directory of the trader was corrupted or lost, this
  command can be used to ask the auction server to send back its view of
  the trader's accounts. This is possible as long as the connected lnd
  node is running with the same seed as when the accounts to recover were
  first created.

  NOTE: This command should only be used after data loss as it will fail
  if there already are open accounts in the trader's database.
  All open or pending orders of any recovered account will be canceled on
  the auctioneer's side and won't be restored in the trader's database.
```

## Auctioneer-Assisted Account Recovery

As part of the initial Lightning Pool launch, an account recovery method has
been added that requires the cooperation of the auctioneer. This method is
based on the account owner sending a recovery intent to the auctioneer, along
with a challenge response to prove ownership. The recovery intent includes a
set of keys derived by the same account BIP-0043 derivation path
`m/1017'/coin_type'/220'/0/index`, so the same `lnd` seed that was to used to
initially create the accounts must be used.

Upon the auctioneer receiving the intent, it will verify the challenge
response, determine the validity of account key to recovery, and provide the
account owner the latest details of the account known to the auctioneer. This
allows the account owner to continue using their account within the auction
without having to perform any on-chain transactions. As usual, the account
owner can decide to close the account and withdraw their funds to their desired
outputs if they no longer wish to participate in the auction.

Note that as a part of this process, all orders belonging to an account being
recovered are canceled within the auction and are not restored to the account
owner.
