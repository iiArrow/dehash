-- wrk script: single hash lookup
-- Usage: wrk -t4 -c50 -d30s -s wrk/lookup.lua http://localhost:8080/lookup
--
-- Picks a random hash from the file on every request.
-- Change HASH_FILE to point at your hash list.

local HASH_FILE = "hashes_amcache.txt"
local DETAILS   = "false"   -- "true" to benchmark with full details

local hashes = {}

function init(args)
    local f = io.open(HASH_FILE, "r")
    if not f then
        error("cannot open " .. HASH_FILE)
    end
    for line in f:lines() do
        local h = line:match("^%s*(.-)%s*$")
        if h ~= "" then
            table.insert(hashes, h)
        end
    end
    f:close()
    math.randomseed(os.time())
    print(string.format("[lookup.lua] loaded %d hashes, details=%s", #hashes, DETAILS))
end

function request()
    local hash = hashes[math.random(#hashes)]
    local body = string.format('{"hash":"%s","details":%s}', hash, DETAILS)
    return wrk.format("POST", "/lookup", {["Content-Type"] = "application/json"}, body)
end
