package keytree

// DevRoot 是设备层根。device 把这里的值当文件路径或 WebSocket URL 直接使用，
// 因此它是写落点校验必须覆盖的域之一。
const DevRoot = PathSegSep + SegDev

func DevTTY(name, stream string) string {
	return PathSegSep + SegDev + PathSegSep + SegTTY + PathSegSep + name + PathSegSep + stream
}
