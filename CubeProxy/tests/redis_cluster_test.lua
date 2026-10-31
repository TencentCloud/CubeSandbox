-- Cluster routing, ASKING, and ambiguous-write regression coverage.
package.path = "./lua/?.lua;" .. package.path
-- OpenResty provides bit. Keep this test runnable on plain Lua as well.
if not pcall(require, "bit") then
    package.preload["bit"] = function()
        local function combine(a, b, xor)
            local result, place = 0, 1
            while a > 0 or b > 0 do
                local x, y = a % 2, b % 2
                if (xor and x ~= y) or (not xor and x == 1 and y == 1) then result = result + place end
                a, b, place = math.floor(a / 2), math.floor(b / 2), place * 2
            end
            return result
        end
        return {band=function(a,b) return combine(a,b,false) end,
            bxor=function(a,b) return combine(a,b,true) end,
            lshift=function(a,b) return a * 2^b end}
    end
end
local store = {}
local dict = {}
function dict:get(k) return store[k] end
function dict:set(k,v) store[k]=v; return true end
function dict:delete(k) store[k]=nil end
ngx = {null={}, shared={redis_cluster_slots=dict}, sleep=function() end, log=function() end, WARN="WARN"}

local state={calls={}, scenario=nil, targets={}}
package.preload["resty.redis"]=function()
    local client={}
    function client:new()
        local o={}
        function o:set_timeout() end
        function o:connect(host,port) self.addr=host..":"..port; state.targets[#state.targets+1]=self.addr; return true end
        function o:auth() return true end
        function o:set_keepalive() return true end
        function o:close() end
        function o:asking() self.asked=true; return true end
        return setmetatable(o,{__index=function(_,cmd)
            return function(self,...)
                state.calls[#state.calls+1]={cmd=cmd,addr=self.addr,args={...},asked=self.asked}
                return state.scenario(self,cmd,{...})
            end
        end})
    end
    return client
end
local cluster=require "redis_cluster"
local opts={redis_cluster_nodes="127.0.0.1:17400,127.0.0.1:17401",redis_index=0,timeout=1000}
assert(cluster.slot("123456789")==12739)
assert(cluster.slot("x{sandbox}a")==cluster.slot("y{sandbox}b"))

state.scenario=function(self,cmd,args)
    if self.addr=="127.0.0.1:17400" then return nil,"MOVED 1 127.0.0.1:17401" end
    return "value"
end
assert(cluster.execute(opts,"get",{"moved-key"})=="value")
local calls=#state.calls
assert(cluster.execute(opts,"get",{"moved-key"})=="value")
assert(#state.calls==calls+1 and state.calls[#state.calls].addr=="127.0.0.1:17401")

state.scenario=function(self)
    if self.addr=="127.0.0.1:17400" then return nil,"ASK 2 127.0.0.1:17402" end
    assert(self.asked,"ASKING must run on the same connection")
    return "asked"
end
assert(cluster.execute(opts,"get",{"ask-key"})=="asked")

local promoted_writes = 0
local readonly_calls = 0
state.scenario = function(self)
    readonly_calls = readonly_calls + 1
    if self.addr == "127.0.0.1:17400" then return nil, "READONLY demoted primary" end
    promoted_writes = promoted_writes + 1
    return promoted_writes
end
assert(cluster.execute(opts, "incr", {"promotion-key"}) == 1)
assert(promoted_writes == 1 and readonly_calls == 2)
local rejected = 0
state.scenario = function() rejected = rejected + 1; return nil, "READONLY replica" end
local _, exhausted = cluster.execute(opts, "incr", {"readonly-only"})
assert(exhausted and rejected == 8, "READONLY retries must remain bounded")

local writes=0
state.scenario=function() writes=writes+1; return nil,"timeout" end
local _,err=cluster.execute(opts,"incr",{"ambiguous-write"})
assert(err=="timeout" and writes==1,"ambiguous writes must not be replayed")

state.scenario=function(_,cmd,args)
    assert(cmd=="mget")
    return {args[1]=="missing" and ngx.null or "value"}
end
local batch=cluster.execute(opts,"mget",{"{a}one","missing","{b}two"})
assert(batch[1]=="value" and batch[2]==ngx.null and batch[3]=="value")

local _,cross=cluster.execute(opts,"eval",{"return 1",2,"{a}one","{b}two"})
assert(cross and cross:find("CROSSSLOT",1,true))
local _,db=cluster.execute({redis_cluster_nodes=opts.redis_cluster_nodes,redis_index=1},"get",{"x"})
assert(db and db:find("DB 0",1,true))

local sent=0
state.scenario=function() sent=sent+1; return "OK" end
local red=require("redis_iresty"):new(opts)
red:init_pipeline()
red:set("{a}first","1")
red:set("{b}second","2")
local results,perr=red:commit_pipeline()
assert(not perr and results[1]=="OK" and results[2]=="OK" and sent==2)
red:init_pipeline()
red:multi()
red:set("{a}transaction","must-not-write")
red:exec()
local _,transaction_error=red:commit_pipeline()
assert(transaction_error and sent==2,"unsupported transactions must fail before any write")
print("Redis Cluster routing tests passed")
