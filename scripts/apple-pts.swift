// apple-pts - prints the presentation and decode time, in seconds, of every video sample of a local MP4 as
// AVFoundation reads it (the media stack AVPlayer plays through), one "pts dts" line a sample, in decode order.
// Run by scripts/apple-check.sh on macOS.
import AVFoundation
import Foundation

func fail(_ message: String) -> Never {
    FileHandle.standardError.write((message + "\n").data(using: .utf8)!)
    exit(1)
}

guard CommandLine.arguments.count == 2 else { fail("usage: apple-pts <file.mp4>") }
let asset = AVURLAsset(url: URL(fileURLWithPath: CommandLine.arguments[1]))
guard let track = asset.tracks(withMediaType: .video).first else { fail("no video track read") }

let reader: AVAssetReader
do { reader = try AVAssetReader(asset: asset) } catch { fail("reader: \(error)") }
let output = AVAssetReaderTrackOutput(track: track, outputSettings: nil) // the stored samples, not decoded
output.alwaysCopiesSampleData = false
reader.add(output)
guard reader.startReading() else { fail("start reading: \(String(describing: reader.error))") }

while let buffer = output.copyNextSampleBuffer() {
    let count = CMSampleBufferGetNumSamples(buffer)
    if count == 0 { continue }
    var timings = [CMSampleTimingInfo](repeating: CMSampleTimingInfo(), count: count)
    var written: CMItemCount = 0
    guard CMSampleBufferGetSampleTimingInfoArray(buffer, entryCount: count, arrayToFill: &timings, entriesNeededOut: &written) == noErr else {
        fail("a sample buffer without timing")
    }
    for t in timings.prefix(Int(written)) {
        let dts = t.decodeTimeStamp.isValid ? t.decodeTimeStamp.seconds : t.presentationTimeStamp.seconds
        print(String(format: "%.6f %.6f", t.presentationTimeStamp.seconds, dts))
    }
}
if reader.status != .completed { fail("reading stopped: \(String(describing: reader.error))") }
