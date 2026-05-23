-- wrk script: bulk hash lookup
-- Usage: wrk -t4 -c20 -d30s -s wrk/bulk.lua http://localhost:8080/bulk
--
-- Builds a fresh random batch on every request.
-- Adjust BATCH_SIZE and DETAILS as needed.

local HASH_FILE  = "hashes_amcache.txt"
local BATCH_SIZE = 100     -- hashes per request
local DETAILS    = "false" -- "true" to benchmark with full details

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
    print(string.format("[bulk.lua] loaded %d hashes, batch=%d, details=%s",
        #hashes, BATCH_SIZE, DETAILS))
end

function request()
    local picked = {}
    for i = 1, BATCH_SIZE do
        picked[i] = string.format('"%s"', hashes[math.random(#hashes)])
    end
    local body = string.format(
        '{"hashes":[%s],"details":%s}',
        table.concat(picked, ","),
        DETAILS
    )
    return wrk.format("POST", "/bulk", {["Content-Type"] = "application/json"}, body)
end

function done(summary, latency, requests)
    local hashes_total = summary.requests * BATCH_SIZE
    io.write(string.format(
        "\n  Hashes processed : %d\n  Hashes/sec       : %.0f\n",
        hashes_total,
        hashes_total / (summary.duration / 1e6)
    ))
end
