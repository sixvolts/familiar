// zip.ts — minimal STORED-method (no compression) zip writer, enough
// for handing Playwright's setInputFiles a real skill archive without
// pulling a zip dependency into the test tree. Go's archive/zip on
// the gateway side reads stored entries fine.

interface ZipEntry {
    name: string;
    data: Buffer;
}

let crcTable: Uint32Array | null = null;

function crc32(buf: Buffer): number {
    if (!crcTable) {
        crcTable = new Uint32Array(256);
        for (let n = 0; n < 256; n++) {
            let c = n;
            for (let k = 0; k < 8; k++) {
                c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
            }
            crcTable[n] = c >>> 0;
        }
    }
    let crc = 0xffffffff;
    for (let i = 0; i < buf.length; i++) {
        crc = crcTable[(crc ^ buf[i]) & 0xff] ^ (crc >>> 8);
    }
    return (crc ^ 0xffffffff) >>> 0;
}

// DOS date for 1980-01-01 — zip's epoch; the gateway ignores mtimes.
const DOS_DATE = 0x21;

export function storedZip(entries: ZipEntry[]): Buffer {
    const localParts: Buffer[] = [];
    const centralParts: Buffer[] = [];
    let offset = 0;
    for (const e of entries) {
        const nameBuf = Buffer.from(e.name, "utf8");
        const crc = crc32(e.data);

        const local = Buffer.alloc(30);
        local.writeUInt32LE(0x04034b50, 0); // local file header
        local.writeUInt16LE(20, 4);         // version needed
        local.writeUInt16LE(0, 8);          // method: stored
        local.writeUInt16LE(DOS_DATE, 12);
        local.writeUInt32LE(crc, 14);
        local.writeUInt32LE(e.data.length, 18);
        local.writeUInt32LE(e.data.length, 22);
        local.writeUInt16LE(nameBuf.length, 26);
        localParts.push(local, nameBuf, e.data);

        const central = Buffer.alloc(46);
        central.writeUInt32LE(0x02014b50, 0); // central directory header
        central.writeUInt16LE(20, 4);         // version made by
        central.writeUInt16LE(20, 6);         // version needed
        central.writeUInt16LE(0, 10);         // method: stored
        central.writeUInt16LE(DOS_DATE, 14);
        central.writeUInt32LE(crc, 16);
        central.writeUInt32LE(e.data.length, 20);
        central.writeUInt32LE(e.data.length, 24);
        central.writeUInt16LE(nameBuf.length, 28);
        central.writeUInt32LE(offset, 42);    // local header offset
        centralParts.push(central, nameBuf);

        offset += 30 + nameBuf.length + e.data.length;
    }
    const centralSize = centralParts.reduce((n, b) => n + b.length, 0);
    const eocd = Buffer.alloc(22);
    eocd.writeUInt32LE(0x06054b50, 0); // end of central directory
    eocd.writeUInt16LE(entries.length, 8);
    eocd.writeUInt16LE(entries.length, 10);
    eocd.writeUInt32LE(centralSize, 12);
    eocd.writeUInt32LE(offset, 16);
    return Buffer.concat([...localParts, ...centralParts, eocd]);
}
