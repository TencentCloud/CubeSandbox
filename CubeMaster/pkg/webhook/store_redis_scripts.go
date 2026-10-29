// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import "github.com/gomodule/redigo/redis"

// Every candidate is decoded, validated and encoded before the first write.
// This prevents predictable partial batches; it cannot roll back Redis OOM or
// unexpected runtime failures. Body lives in the opaque input string (base64 in
// its Go JSON encoding), never decoded/re-encoded as an external event by Lua.
var deliveryScript = redis.NewScript(3, `
local tasks,due,terminal = KEYS[1],KEYS[2],KEYS[3]
for i,want in ipairs({'hash','zset','zset'}) do
 local kind=redis.call('TYPE',KEYS[i]).ok
 assert(kind=='none' or kind==want,'CORRUPT key type')
end
local function integer(n,lo,hi)
 return type(n)=='number' and n==math.floor(n) and n>=lo and n<=hi
end
local function number(s,lo,hi)
 local n=tonumber(s); assert(integer(n,lo,hi),'INVALID number'); return n
end
local function text(s,max) return type(s)=='string' and #s<=max end
local function summary(s)
 return type(s)=='table' and text(s.Code,64) and text(s.Message,1024) and
 (s.HTTPStatus==0 or integer(s.HTTPStatus,100,599))
end
local function ended(s) return s=='succeeded' or s=='failed' or s=='cancelled' end
local function validateInput(raw,max)
 -- Validate the immutable envelope too, without interpreting the body bytes.
 local input=cjson.decode(raw)
 assert(type(input)=='table' and text(input.ID,36) and #input.ID==36 and
 text(input.Type,64) and text(input.Body,349528) and #input.Body>0 and
 text(input.SubscriptionID,1024) and #input.SubscriptionID>0 and
 text(input.TargetURL,4096) and #input.TargetURL>0 and input.MaxAttempts==max and
 text(input.OccurredAt,64) and text(input.CreatedAt,64) and text(input.NextAttemptAt,64),'CORRUPT input')
end
local function read(key)
 local raw=redis.call('HGET',tasks,key)
 assert(raw and #raw<=524288,'CORRUPT missing/oversized record')
 local r=cjson.decode(raw)
 assert(type(r)=='table' and text(r.input,393216) and #r.input>0 and
 integer(r.max,1,1000) and integer(r.attempts,0,r.max) and
 integer(r.generation,0,1000) and r.generation==r.attempts and text(r.token,36) and
 integer(r.next,1,9999999999999) and integer(r.lease,0,9999999999999) and
 integer(r.finished,0,9999999999999) and summary(r.failure) and
 type(r.result)=='table' and summary(r.result.Summary),'CORRUPT record')
 assert((r.state=='pending' and r.lease==0 and r.token=='' and r.finished==0) or
 (r.state=='inflight' and r.lease>0 and #r.token==36 and r.attempts>0 and r.finished==0) or
 (ended(r.state) and r.lease==0 and r.token=='' and r.finished>0 and r.result.State==r.state), 'CORRUPT state')
 validateInput(r.input,r.max)
 local dueScore=redis.call('ZSCORE',due,key)
 local terminalScore=redis.call('ZSCORE',terminal,key)
 if ended(r.state) then
  assert(dueScore==false and terminalScore and tonumber(terminalScore)>=r.finished,'CORRUPT terminal index')
 else
  local expected=r.next; if r.state=='inflight' then expected=r.lease end
  assert(terminalScore==false and tonumber(dueScore)==expected,'CORRUPT active index')
 end
 return r
end
local function encode(r)
 local raw=cjson.encode(r); assert(#raw<=524288,'CORRUPT record size'); return raw
end
local empty={Code='',Message='',HTTPStatus=0}
local function finish(r,now,result)
 r.state=result.State; r.result=result; r.finished=now; r.lease=0; r.token=''
end
local exhausted={State='failed',Summary={Code='attempts_exhausted',Message='',HTTPStatus=0}}
local op=ARGV[1]
if op=='put' then
 local key,input=ARGV[2],ARGV[3]
 local max=number(ARGV[4],1,1000); local next=number(ARGV[5],1,9999999999999)
 local capacity=number(ARGV[6],1,9007199254740991)
 assert(text(key,1072) and text(input,393216),'INVALID input')
 if redis.call('HEXISTS',tasks,key)==1 then
  local r=read(key)
  if r.input~=input then return redis.error_reply('CONFLICT') end
  return 0
 end
 assert(redis.call('ZSCORE',due,key)==false and redis.call('ZSCORE',terminal,key)==false,'CORRUPT orphan index')
 if redis.call('HLEN',tasks)>=capacity then return redis.error_reply('CAPACITY') end
 local r={input=input,max=max,attempts=0,generation=0,token='',state='pending',next=next,lease=0,finished=0,failure=empty,result={State='',Summary=empty}}
 validateInput(input,max)
 local raw=encode(r)
 redis.call('HSET',tasks,key,raw); redis.call('ZADD',due,next,key)
 return 1
elseif op=='sample' then
 local oldest=redis.call('ZRANGE',due,0,0,'WITHSCORES')
 return {redis.call('ZCARD',due),oldest[2] or '0'}
elseif op=='claim' or op=='prune' then
 local now=number(ARGV[2],1,9999999999999); local limit=number(ARGV[3],1,32)
 local lease,token,retention
 if op=='claim' then
  lease=number(ARGV[4],1,9999999999999-now); token=ARGV[5]
  assert(text(token,36) and #token==36,'INVALID token')
  retention=number(ARGV[6],1,9999999999999-now)
 end
 local index=due; if op=='prune' then index=terminal end
 local keys=redis.call('ZRANGEBYSCORE',index,'-inf',now,'LIMIT',0,limit)
 local records,encoded={},{}
 local reclaimed,exhaustedCount=0,0
 for i,key in ipairs(keys) do
  local r=read(key)
  if op=='prune' then
   assert(ended(r.state),'CORRUPT nonterminal prune candidate')
  else
   assert(r.state=='pending' or r.state=='inflight','CORRUPT due state')
   local score=r.next; if r.state=='inflight' then score=r.lease end
   assert(tonumber(redis.call('ZSCORE',due,key))==score and score<=now,'CORRUPT due score')
   if r.attempts>=r.max then finish(r,now,exhausted); exhaustedCount=exhaustedCount+1
   else
    if r.state=='inflight' then reclaimed=reclaimed+1 end
    r.state='inflight'; r.attempts=r.attempts+1; r.generation=r.generation+1
    r.token=token; r.lease=now+lease
   end
   encoded[i]=encode(r)
  end
  records[i]=r
 end
 local out={}
 for i,key in ipairs(keys) do
  local r=records[i]
  if op=='prune' then
   redis.call('HDEL',tasks,key); redis.call('ZREM',terminal,key)
  else
   redis.call('HSET',tasks,key,encoded[i])
   if ended(r.state) then
    redis.call('ZREM',due,key); redis.call('ZADD',terminal,now+retention,key)
   else
    redis.call('ZADD',due,r.lease,key); out[#out+1]=key; out[#out+1]=encoded[i]
   end
  end
 end
 if op=='prune' then return #keys end
 table.insert(out,1,tostring(exhaustedCount)); table.insert(out,1,tostring(reclaimed))
 return out
elseif op=='complete' or op=='retry' then
 local key=ARGV[2]; local generation=number(ARGV[3],1,1000); local token=ARGV[4]
 local now=number(ARGV[5],1,9999999999999)
 local retention=number(ARGV[6],1,9999999999999-now)
 local value=cjson.decode(ARGV[7]); local next
 if op=='complete' then assert(type(value)=='table' and ended(value.State) and summary(value.Summary),'INVALID result')
 else assert(summary(value),'INVALID summary'); next=number(ARGV[8],1,9999999999999) end
 if redis.call('HEXISTS',tasks,key)==0 then return redis.error_reply('STALE') end
 local r=read(key)
 if r.state~='inflight' or r.generation~=generation or r.token~=token then return redis.error_reply('STALE') end
 if op=='complete' then finish(r,now,value)
 else
  r.failure=value
  if r.attempts>=r.max then finish(r,now,exhausted)
  else r.state='pending'; r.next=next; r.lease=0; r.token='' end
 end
 local raw=encode(r)
 redis.call('HSET',tasks,key,raw)
 if ended(r.state) then redis.call('ZREM',due,key); redis.call('ZADD',terminal,now+retention,key)
 else redis.call('ZADD',due,r.next,key) end
 return 1
end
return redis.error_reply('INVALID')
`)
