-- wrk script: single hash lookup
-- Usage: wrk -t4 -c50 -d30s -s wrk/lookup.lua http://localhost:8080/lookup \
--          -- --hashfile hashes.txt [--details true]
--
-- Picks a random hash from the file on every request.

local hashes = {}
local DETAILS = "false"

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
        error("usage: wrk ... -- --hashfile <path> [--details true|false]")
    end
    DETAILS = flags["details"] or "false"

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
    print(string.format("[lookup.lua] loaded %d hashes from %s, details=%s",
        #hashes, flags["hashfile"], DETAILS))
end

function request()
    local hash = hashes[math.random(#hashes)]
    local body = string.format('{"hash":"%s","details":%s}', hash, DETAILS)
    return wrk.format("POST", "/lookup", {["Content-Type"] = "application/json"}, body)
end
