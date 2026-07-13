# frozen_string_literal: true

require "json"
require "fileutils"
require "sinatra"

set :bind, "0.0.0.0"
set :port, Integer(ENV.fetch("PORT", "4567"))
set :server, :puma

WORKSPACE = ENV.fetch("WORKSPACE", "/workspace")
COUNTER_FILE = File.join(WORKSPACE, "data", "counter.txt")

configure do
  FileUtils.mkdir_p(File.dirname(COUNTER_FILE))
end

def counter
  File.open(COUNTER_FILE, File::RDWR | File::CREAT, 0o644) do |file|
    file.flock(File::LOCK_SH)
    contents = file.read.strip
    contents.empty? ? 0 : Integer(contents)
  end
end

def increment_counter
  File.open(COUNTER_FILE, File::RDWR | File::CREAT, 0o644) do |file|
    file.flock(File::LOCK_EX)
    contents = file.read.strip
    value = (contents.empty? ? 0 : Integer(contents)) + 1
    file.rewind
    file.write("#{value}\n")
    file.truncate(file.pos)
    value
  end
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
  content_type :json
  { counter: increment_counter }.to_json
end
