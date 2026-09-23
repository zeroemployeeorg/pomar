import Foundation
import PomarHostCore

let args = Array(CommandLine.arguments.dropFirst())
switch args.first {
case "version":
    print("pomar-host \(HostInfo.version) containerization \(HostInfo.containerizationVersion)")
    exit(0)
case "probe-vmnet" where args.count == 2 && (args[1] == "host" || args[1] == "shared"):
    let r = VmnetProbe.run(hostOnly: args[1] == "host")
    print(r.detail)
    exit(r.ok ? 0 : 1)
default:
    FileHandle.standardError.write(Data("usage: pomar-host version | probe-vmnet host|shared\n".utf8))
    exit(2)
}
