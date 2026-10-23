# 生成 EXE 使用的麦克风图标；运行时的状态图标由 Go 原生绘制。
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing
$taskBitmap = New-Object System.Drawing.Bitmap 64, 64
try {
    for ($taskY = 0; $taskY -lt 64; $taskY++) {
        for ($taskX = 0; $taskX -lt 64; $taskX++) {
            $taskDx = $taskX - 32
            $taskDy = $taskY - 32
            if ($taskDx * $taskDx + $taskDy * $taskDy -le 961) {
                $taskBitmap.SetPixel($taskX, $taskY, [System.Drawing.Color]::FromArgb(255, 196, 58, 67))
            }
            $taskTop = $taskY - 19
            $taskBottom = $taskY - 30
            $taskBody = ($taskDx * $taskDx + $taskTop * $taskTop -le 49) -or ($taskDx * $taskDx + $taskBottom * $taskBottom -le 49) -or ($taskX -ge 25 -and $taskX -le 39 -and $taskY -ge 19 -and $taskY -le 30)
            $taskArcY = $taskY - 30
            $taskDist = $taskDx * $taskDx + $taskArcY * $taskArcY
            $taskArc = $taskY -ge 30 -and $taskDist -ge 196 -and $taskDist -le 289
            $taskSides = (($taskX -ge 15 -and $taskX -le 18) -or ($taskX -ge 46 -and $taskX -le 49)) -and $taskY -ge 25 -and $taskY -le 30
            $taskStem = $taskX -ge 30 -and $taskX -le 34 -and $taskY -ge 46 -and $taskY -le 54
            $taskBase = $taskX -ge 23 -and $taskX -le 41 -and $taskY -ge 53 -and $taskY -le 56
            if ($taskBody -or $taskArc -or $taskSides -or $taskStem -or $taskBase) {
                $taskBitmap.SetPixel($taskX, $taskY, [System.Drawing.Color]::White)
            }
        }
    }
    $taskPNG = New-Object System.IO.MemoryStream
    try {
        $taskBitmap.Save($taskPNG, [System.Drawing.Imaging.ImageFormat]::Png)
        $taskData = $taskPNG.ToArray()
        $taskICO = [System.IO.File]::Create((Join-Path $PSScriptRoot 'app.ico'))
        $taskWriter = New-Object System.IO.BinaryWriter $taskICO
        try {
            # ICO 文件头与单张 PNG 图像目录项。
            $taskWriter.Write([uint16]0)
            $taskWriter.Write([uint16]1)
            $taskWriter.Write([uint16]1)
            $taskWriter.Write([byte]64)
            $taskWriter.Write([byte]64)
            $taskWriter.Write([byte]0)
            $taskWriter.Write([byte]0)
            $taskWriter.Write([uint16]1)
            $taskWriter.Write([uint16]32)
            $taskWriter.Write([uint32]$taskData.Length)
            $taskWriter.Write([uint32]22)
            $taskWriter.Write($taskData)
        } finally {
            $taskWriter.Dispose()
        }
    } finally {
        $taskPNG.Dispose()
    }
} finally {
    $taskBitmap.Dispose()
}
