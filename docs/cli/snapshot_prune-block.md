# Prune ancient blockchain

The ```bor snapshot prune-block``` command will prune historical blockchain data stored in the ancientdb. The amount of blocks expected for remaining after prune can be specified via `block-amount-reserved` in this command, will prune and only remain the specified amount of old block data in ancientdb.


The brief workflow as below:

1. backup the the number of specified number of blocks backward in original ancientdb into new ancient_backup,
2. then delete the original ancientdb dir and rename the ancient_backup to original one for replacement,
3. finally assemble the statedb and new ancientdb together.

The purpose of doing it is because the block data will be moved into the ancient store when it becomes old enough(exceed the Threshold 90000), the disk usage will be very large over time, and is occupied mainly by ancientdb, so it's very necessary to do block data pruning, this feature will handle it.

## Options

- ```datadir```: Path of the data directory to store information

- ```keystore```: Path of the data directory to store keys

- ```datadir.ancient```: Path of the old ancient data directory

- ```block-amount-reserved```: Sets the expected reserved number of blocks for offline block prune (default: 1024)

- ```cache.triesinmemory```: Number of block states (tries) to keep in memory (default = 128) (default: 128)

- ```check-snapshot-with-mpt```: Enable checking between snapshot and MPT (default: false)

### Cache Options

- ```cache```: Megabytes of memory allocated to internal caching (default: 1024)