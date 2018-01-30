package utilities

import (
	"path/filepath"
)

// IsStaticFile returns true if a path has a file
// extension which we will assume to be static.
func IsStaticFile(path string) bool {
	isFile, found := fileExtensions[filepath.Ext(path)]
	return found && isFile
}

var fileExtensions = map[string]bool{
	".7z":    true,
	".avi":   true,
	".bmp":   true,
	".bz2":   true,
	".css":   true,
	".csv":   true,
	".doc":   true,
	".docx":  true,
	".eot":   true,
	".flac":  true,
	".flv":   true,
	".gif":   true,
	".gz":    true,
	".ico":   true,
	".jpeg":  true,
	".jpg":   true,
	".js":    true,
	".less":  true,
	".mka":   true,
	".mkv":   true,
	".mov":   true,
	".mp3":   true,
	".mp4":   true,
	".mpeg":  true,
	".mpg":   true,
	".odt":   true,
	".otf":   true,
	".ogg":   true,
	".ogm":   true,
	".opus":  true,
	".pdf":   true,
	".png":   true,
	".ppt":   true,
	".pptx":  true,
	".rar":   true,
	".rtf":   true,
	".svg":   true,
	".svgz":  true,
	".swf":   true,
	".tar":   true,
	".tbz":   true,
	".tgz":   true,
	".ttf":   true,
	".txt":   true,
	".txz":   true,
	".wav":   true,
	".webm":  true,
	".webp":  true,
	".woff":  true,
	".woff2": true,
	".xls":   true,
	".xlsx":  true,
	".xml":   true,
	".xz":    true,
	".zip":   true,
}
