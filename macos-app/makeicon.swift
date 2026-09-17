// makeicon.swift — renders an .iconset (a blue "portal" ring) for OpenFlux.app.
// Usage: makeicon <out-base>   -> writes <out-base>.iconset/ with all sizes.
import AppKit

let outBase = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "/tmp/oflx-icon"
let iconset = outBase + ".iconset"
try? FileManager.default.createDirectory(atPath: iconset, withIntermediateDirectories: true)

func render(_ px: Int) -> Data {
    let s = CGFloat(px)
    let img = NSImage(size: NSSize(width: s, height: s))
    img.lockFocus()
    let ctx = NSGraphicsContext.current!.cgContext
    ctx.clear(CGRect(x: 0, y: 0, width: s, height: s))

    // Rounded-square background so it reads as a macOS app icon.
    let bgInset = s * 0.06
    let bgRect = CGRect(x: bgInset, y: bgInset, width: s - 2*bgInset, height: s - 2*bgInset)
    let bg = NSBezierPath(roundedRect: bgRect, xRadius: s*0.22, yRadius: s*0.22)
    NSColor(calibratedRed: 0.10, green: 0.15, blue: 0.22, alpha: 1).setFill()
    bg.fill()

    // Blue ring (donut) — the "tunnel portal".
    let ringInset = s * 0.26
    let ringRect = CGRect(x: ringInset, y: ringInset, width: s - 2*ringInset, height: s - 2*ringInset)
    ctx.setFillColor(NSColor.systemBlue.cgColor)
    ctx.fillEllipse(in: ringRect)
    let hole = s * 0.24
    let hRect = CGRect(x: (s-hole)/2, y: (s-hole)/2, width: hole, height: hole)
    ctx.setBlendMode(.clear)
    ctx.fillEllipse(in: hRect)
    ctx.setBlendMode(.normal)

    img.unlockFocus()
    let tiff = img.tiffRepresentation!
    let rep = NSBitmapImageRep(data: tiff)!
    rep.size = NSSize(width: s, height: s)
    return rep.representation(using: .png, properties: [:])!
}

let sizes: [(String, Int)] = [
    ("16x16", 16), ("16x16@2x", 32),
    ("32x32", 32), ("32x32@2x", 64),
    ("128x128", 128), ("128x128@2x", 256),
    ("256x256", 256), ("256x256@2x", 512),
    ("512x512", 512), ("512x512@2x", 1024),
]
for (name, px) in sizes {
    let data = render(px)
    try! data.write(to: URL(fileURLWithPath: iconset + "/icon_\(name).png"))
}
print(iconset)
