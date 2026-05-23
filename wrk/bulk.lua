-- wrk script: bulk hash lookup
-- Usage: wrk -t4 -c20 -d30s -s wrk/bulk.lua http://localhost:8080/bulk \
--          -- --hashfile hashes.txt [--batchsize 100] [--details true]
--
-- Builds a fresh random batch on every request.

local hashes     = {}
local BATCH_SIZE = 100
local DETAILS    = "false"

local function parse_args(args)
    local flags = {}
    local i = 1
    while i <= #args do
        local key = args[i]:match("^%-%-(.+)$")
        if key and args[i+1] then
            flags[key] = args[i+1]
            i = i + 2
        else
            i = i + 1
        end
    end
    return flags
end

function init(args)
    local flags = parse_args(args)

    if not flags["hashfile"] then
        error("usage: wrk ... -- --hashfile <path> [--batchsize 100] [--details true|false]")
    end
    BATCH_SIZE = tonumber(flags["batchsize"]) or 100
    DETAILS    = flags["details"] or "false"

    local f = io.open(flags["hashfile"], "r")
    if not f then
        error("cannot open " .. flags["hashfile"])
    end
    for line in f:lines() do
        local h = line:match("^%s*(.-)%s*$")
        if h ~= "" then
            table.insert(hashes, h)
        end
    end
    f:close()
    math.randomseed(os.time())
    print(string.format("[bulk.lua] loaded %d hashes from %s, batch=%d, details=%s",
        #hashes, flags["hashfile"], BATCH_SIZE, DETAILS))
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
