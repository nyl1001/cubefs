package console

import (
	"testing"
)

func TestMakeHtml2GoBin(t *testing.T) {
	// when you need rebuild html . please open it
	/*
		assets := getAssets()

		err := vfsgen.Generate(assets, vfsgen.Options{
			PackageName:  "console",
			BuildTags:    "!dev",
			VariableName: "Assets",
		})

		if err != nil {
			log.Fatalln(err)
		}*/
}

// func getAssets() http.FileSystem {
// 	_, fileStr, _, _ := runtime.Caller(1)
// 	split, _ := path.Split(fileStr)

// 	var assets http.FileSystem = http.Dir(split + "html")
// 	return assets
// }
