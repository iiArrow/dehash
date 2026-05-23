-- wrk script: file-based lookup (/file endpoint)
-- Usage: wrk -t2 -c10 -d30s -s wrk/file.lua http://localhost:8080 \
--          -- --hashfile hashes.txt [--details true]
--
-- Sends a fixed plain-text body (all hashes at once) to /file.
-- Suitable for benchmarking the large-file path.

local body    = ""
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
    body = f:read("*a")
    f:close()

    local count = 0
    for _ in body:gmatch("[^\n]+") do count = count + 1 end
    print(string.format("[file.lua] loaded %d hashes from %s, details=%s",
        count, flags["hashfile"], DETAILS))
end

function request()
    local path = "/file?details=" .. DETAILS
    return wrk.format("POST", path, {["Content-Type"] = "text/plain"}, body)
end
