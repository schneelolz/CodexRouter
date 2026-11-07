// Generate a complete ICNS family from the existing CodexRouter blue routing mark.
// AppKit/CoreGraphics only; no downloaded artwork or build-time package dependencies.
import AppKit
import Foundation

if CommandLine.arguments.count != 2 { fatalError("Usage: swift macos-icon.swift <output.iconset>") }
let output = URL(fileURLWithPath: CommandLine.arguments[1], isDirectory: true)
try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
let space = CGColorSpaceCreateDeviceRGB()
func image(_ size: Int) throws -> Data {
    guard let canvas = CGContext(data: nil, width: size, height: size, bitsPerComponent: 8,
        bytesPerRow: size * 4, space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else {
        throw NSError(domain: "IconCanvas", code: 1)
    }
    canvas.scaleBy(x: CGFloat(size) / 1024, y: CGFloat(size) / 1024)
    canvas.setAllowsAntialiasing(true)
    let blue = CGColor(red: 36/255, green: 93/255, blue: 168/255, alpha: 1)
    canvas.setFillColor(blue)
    canvas.addPath(CGPath(roundedRect: CGRect(x: 96, y: 96, width: 832, height: 832),
        cornerWidth: 180, cornerHeight: 180, transform: nil))
    canvas.fillPath()
    canvas.setStrokeColor(CGColor(gray: 1, alpha: 1))
    canvas.setLineWidth(52)
    canvas.setLineCap(.round)
    canvas.setLineJoin(.round)
    // The routing tree matches frontend/public/favicon.svg, flipped into CoreGraphics coordinates.
    canvas.move(to: CGPoint(x: 320, y: 720)); canvas.addLine(to: CGPoint(x: 320, y: 304))
    canvas.move(to: CGPoint(x: 320, y: 512)); canvas.addLine(to: CGPoint(x: 515, y: 512))
    canvas.addLine(to: CGPoint(x: 710, y: 707))
    canvas.move(to: CGPoint(x: 515, y: 512)); canvas.addLine(to: CGPoint(x: 710, y: 317))
    canvas.strokePath()
    for point in [CGPoint(x: 320, y: 720), CGPoint(x: 710, y: 707), CGPoint(x: 710, y: 317)] {
        canvas.setFillColor(blue)
        canvas.addEllipse(in: CGRect(x: point.x-52, y: point.y-52, width: 104, height: 104))
        canvas.drawPath(using: .fillStroke)
    }
    guard let result = canvas.makeImage() else { throw NSError(domain: "IconImage", code: 1) }
    let bitmap = NSBitmapImageRep(cgImage: result)
    guard let png = bitmap.representation(using: .png, properties: [:]) else { throw NSError(domain: "IconPNG", code: 1) }
    // Check the rendered center and transparent margin, not just PNG dimensions.
    guard let pixels = canvas.data?.assumingMemoryBound(to: UInt8.self) else { throw NSError(domain: "IconPixels", code: 1) }
    let center = ((size / 2) * size + size / 2) * 4
    if pixels[3] != 0 || pixels[center + 3] == 0 { throw NSError(domain: "IconBlank", code: 1) }
    return png
}
for size in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let suffix = scale == 2 ? "@2x" : ""
        try image(size * scale).write(to: output.appendingPathComponent("icon_\(size)x\(size)\(suffix).png"))
    }
}
print("App icon: 10 PNG representations rendered; alpha and center pixel checks passed")
