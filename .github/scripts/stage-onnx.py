"""Stage verified native runtime files beside kennembed, including notices."""
import argparse
import hashlib
import os
import pathlib
import platform
import shutil
import subprocess
import zipfile

# Retry HTTP refusals such as 403 as well as curl's default transient errors.
curl = ['curl', '--silent', '--show-error', '--fail', '--location', '--retry', '2', '--retry-all-errors', '--retry-delay', '5', '--connect-timeout', '30', '--speed-limit', '1', '--speed-time', '60', '--remove-on-error']

# The package contains native libraries for every supported target.
targets = {
    ('linux', 'amd64'): ('linux-x64', 'libonnxruntime.so', 'libonnxruntime.so.1.23.2', '13ab8084954fa4a47c777880180b90810d6020f021441395712b48a75b74c68b', '086ec1d5388f64153d9c63470d126693db9a182c8ce236d3a1119068471b8a0d'),
    ('linux', 'arm64'): ('linux-arm64', 'libonnxruntime.so', 'libonnxruntime.so.1.23.2', '648ffa64fbe027ae27139109410900cf776a030dec2dbbac51053318cc44c286', '3b6be288fbfb7dff8770d08a23defdde18e8f7e0f5a2b344a0e5e238c999ea88'),
    ('darwin', 'amd64'): ('osx-x64', 'libonnxruntime.dylib', 'libonnxruntime.1.23.2.dylib', '8c9c78de65ea3786f987c0d980e9c1b13a3a5fbc6b3e2965ba05b450e6e4c054', None),
    ('darwin', 'arm64'): ('osx-arm64', 'libonnxruntime.dylib', 'libonnxruntime.1.23.2.dylib', 'd306d2bc768540766c7ed8a1e0ff05d2870c77a934ebeee4a7bafa1b732ef299', None),
    ('windows', 'amd64'): ('win-x64', 'onnxruntime.dll', 'onnxruntime.dll', 'dec964ab1ee36cc9b0ae247d13b376627992fc57dec0454354017ab8fd84f1ea', 'a2b3a50956aa75a9879c8472bc7df4f7a8072bcd2db19a1b7d988e7688f293ef'),
    ('windows', 'arm64'): ('win-arm64', 'onnxruntime.dll', 'onnxruntime.dll', '99a11bb077f723a81d100fd98099f332e4178bbb7025ac10fd40699a0b82c2f0', '27cafd6da30843bf4c5f42f1af16cdd42af4aca69f9003bb3ef7382d8c794978'),
}
parser = argparse.ArgumentParser()
parser.add_argument('--os', default=platform.system().lower())
parser.add_argument('--arch', default='arm64' if platform.machine().lower() in ('arm64', 'aarch64') else 'amd64')
parser.add_argument('--output', type=pathlib.Path, required=True)
parser.add_argument('--cache-dir', type=pathlib.Path)
args = parser.parse_args()
target, library, output_library, digest, provider_digest = targets[(args.os, args.arch)]
root = args.output
root.mkdir(parents=True, exist_ok=True)
cache = args.cache_dir or root.parent
cache.mkdir(parents=True, exist_ok=True)
archive = cache / 'microsoft.ml.onnxruntime.1.23.2.nupkg'
if not archive.exists():
    subprocess.run(curl + ['--output', str(archive), 'https://api.nuget.org/v3-flatcontainer/microsoft.ml.onnxruntime/1.23.2/' + archive.name], check=True)

files = {
    'runtimes/' + target + '/native/' + library: (output_library, digest),
    'LICENSE': ('LICENSE', 'c250d6278f0b47a6439fb7592b08b58a55eb9f535aa49a1db63211c3f982b674'),
    'ThirdPartyNotices.txt': ('ThirdPartyNotices.txt', 'e00f828e0a33de591a355ae6606d2625f5758da7d2c844db7821c9dd3e3647b6'),
}
if provider_digest:
    provider = 'onnxruntime_providers_shared.dll' if args.os == 'windows' else 'libonnxruntime_providers_shared.so'
    files['runtimes/' + target + '/native/' + provider] = (provider, provider_digest)
with zipfile.ZipFile(archive) as source:
    for entry, (name, expected) in files.items():
        data = source.read(entry)
        if hashlib.sha256(data).hexdigest() != expected:
            raise RuntimeError('ONNX Runtime SHA-256 mismatch: ' + entry)
        (root / name).write_bytes(data)
if args.os == 'windows':
    # Use the licensed Visual Studio redistribution files, never System32 copies.
    vswhere = pathlib.Path(os.environ['ProgramFiles(x86)']) / 'Microsoft Visual Studio/Installer/vswhere.exe'
    installation = subprocess.check_output([str(vswhere), '-latest', '-products', '*', '-property', 'installationPath'], text=True).strip()
    architecture = 'x64' if args.arch == 'amd64' else 'arm64'
    candidates = sorted((pathlib.Path(installation) / 'VC/Redist/MSVC').glob('*/' + architecture + '/Microsoft.VC*.CRT'))
    if not candidates:
        raise RuntimeError('Visual C++ redistribution files are missing for ' + architecture)
    for dll in ('msvcp140.dll', 'msvcp140_1.dll', 'vcruntime140.dll', 'vcruntime140_1.dll'):
        shutil.copy2(candidates[-1] / dll, root / dll)
    subprocess.run(curl + ['--output', str(root / 'Visual-C++-Runtime-License.html'), 'https://aka.ms/VCRedistLicense'], check=True)
# The server's optional face models retain their distribution notices.
for notice in ('AuraFace-LICENSE', 'YuNet-LICENSE'):
    shutil.copy2(pathlib.Path('.github/licenses') / notice, root / notice)
print(root / output_library)
