-- CubeSandbox cube-egress — init_worker phase entry point.
math.randomseed(ngx.now() * 1000 + ngx.worker.id())

-- bootstrap.run() is a no-op on workers other than worker 0; on worker 0
-- it schedules a ngx.timer to fetch CUBE_EGRESS_BOOTSTRAP_URL and load
-- policies into shared memory
local bootstrap = require "bootstrap"
bootstrap.run()
