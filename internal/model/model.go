package model

type FileDetails struct {
	SHA256       string `json:"sha256"        msgpack:"a"`
	SHA1         string `json:"sha1"          msgpack:"b"`
	MD5          string `json:"md5"           msgpack:"c"`
	CRC32        string `json:"crc32"         msgpack:"d"`
	FileName     string `json:"file_name"     msgpack:"e"`
	FileSize     int64  `json:"file_size"     msgpack:"f"`
	Path         string `json:"path"          msgpack:"g"`
	AppName      string `json:"app_name"      msgpack:"h"`
	AppVersion   string `json:"app_version"   msgpack:"i"`
	AppType      string `json:"app_type"      msgpack:"j"`
	Manufacturer string `json:"manufacturer"  msgpack:"k"`
	OSName       string `json:"os_name"       msgpack:"l"`
	OSVersion    string `json:"os_version"    msgpack:"m"`
	Language     string `json:"language"      msgpack:"n"`
}
