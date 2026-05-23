-- wrk script: file-based lookup (/file endpoint)
-- Usage: wrk -t2 -c10 -d30s -s wrk/file.lua http://localhost:8080
--
-- Sends a fixed plain-text body (all hashes at once) to /file.
-- Suitable for benchmarking the large-file path.

local HASH_FILE = "hashes_amcache.txt"
local DETAILS   = "false"  -- "true" to include details in response

local body = ""

function init(args)
    local f = io.open(HASH_FILE, "r")
    if not f then
        error("cannot open " .. HASH_FILE)
    end
    body = f:read("*a")
    f:close()

    local count = 0
    for _ in body:gmatch("[^\n]+") do count = count + 1 end
    print(string.format("[file.lua] loaded %d hashes, details=%s", count, DETAILS))
end

function request()
    local path = "/file?details=" .. DETAILS
    return wrk.format("POST", path, {["Content-Type"] = "text/plain"}, body)
end
