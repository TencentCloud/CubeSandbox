# drain() fix note

Old harness destroyed every 32-hex ID from `cli list` after CREATED drain.
Repair copy only destroys CREATED/registry IDs; leftover IDs abort new load.
Not executed on live cluster in this repair sprint.
