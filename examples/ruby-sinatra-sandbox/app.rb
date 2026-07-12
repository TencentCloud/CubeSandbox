# frozen_string_literal: true

require "json"
require "fileutils"
require "sinatra"

set :bind, "0.0.0.0"
set :port, Integer(ENV.fetch("PORT", "4567"))
set :server, :puma

WORKSPACE = ENV.fetch("WORKSPACE", "/workspace")
COUNTER_FILE = File.join(WORKSPACE, "data", "counter.txt")

def counter
  FileUtils.mkdir_p(File.dirname(COUNTER_FILE))
  File.exist?(COUNTER_FILE) ? Integer(File.read(COUNTER_FILE).strip) : 0
end

get "/health" do
  content_type :json
  { status: "ok", ruby: RUBY_VERSION }.to_json
end

get "/counter" do
  content_type :json
  { counter: counter }.to_json
end

post "/counter" do
  value = counter + 1
  File.write(COUNTER_FILE, "#{value}\n")
  content_type :json
  { counter: value }.to_json
end
