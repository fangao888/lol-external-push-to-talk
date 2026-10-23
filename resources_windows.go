package main

// 生成带 Common Controls 6 清单的原生资源；正常编译可复用已生成的 syso 文件。
//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -manifest app.manifest -ico assets/app.ico -o resources_windows_amd64.syso
//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch 386 -manifest app.manifest -ico assets/app.ico -o resources_windows_386.syso
