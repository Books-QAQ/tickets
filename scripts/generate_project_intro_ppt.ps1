param(
    [string]$OutputPath = ".\tickets-project-intro-3slides.pptx"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Write-Utf8File {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Content
    )

    $directory = Split-Path -Parent $Path
    if ($directory -and -not (Test-Path $directory)) {
        New-Item -ItemType Directory -Path $directory -Force | Out-Null
    }

    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, $Content, $utf8NoBom)
}

function Escape-Xml {
    param([string]$Text)
    return [System.Security.SecurityElement]::Escape($Text)
}

function New-TextParagraphXml {
    param(
        [string]$Text,
        [int]$FontSize = 2200,
        [bool]$Bold = $false
    )

    $escaped = Escape-Xml $Text
    $boldAttr = if ($Bold) { ' b="1"' } else { "" }
    return @"
<a:p>
  <a:r>
    <a:rPr lang="zh-CN" sz="$FontSize"$boldAttr dirty="0" smtClean="0"/>
    <a:t>$escaped</a:t>
  </a:r>
  <a:endParaRPr lang="zh-CN" sz="$FontSize"/>
</a:p>
"@
}

function New-ParagraphBlockXml {
    param(
        [string[]]$Lines,
        [int]$FontSize = 2200
    )

    return ($Lines | ForEach-Object { New-TextParagraphXml -Text $_ -FontSize $FontSize }) -join "`n"
}

function New-SlideXml {
    param(
        [string]$Title,
        [string[]]$Lines
    )

    $titleXml = New-TextParagraphXml -Text $Title -FontSize 2800 -Bold $true
    $bodyXml = New-ParagraphBlockXml -Lines $Lines -FontSize 2200

    return @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld>
    <p:spTree>
      <p:nvGrpSpPr>
        <p:cNvPr id="1" name=""/>
        <p:cNvGrpSpPr/>
        <p:nvPr/>
      </p:nvGrpSpPr>
      <p:grpSpPr>
        <a:xfrm>
          <a:off x="0" y="0"/>
          <a:ext cx="0" cy="0"/>
          <a:chOff x="0" y="0"/>
          <a:chExt cx="0" cy="0"/>
        </a:xfrm>
      </p:grpSpPr>
      <p:sp>
        <p:nvSpPr>
          <p:cNvPr id="2" name="Title"/>
          <p:cNvSpPr txBox="1"/>
          <p:nvPr/>
        </p:nvSpPr>
        <p:spPr>
          <a:xfrm>
            <a:off x="685800" y="457200"/>
            <a:ext cx="10820400" cy="914400"/>
          </a:xfrm>
          <a:prstGeom prst="rect"><a:avLst/></a:prstGeom>
          <a:noFill/>
          <a:ln><a:noFill/></a:ln>
        </p:spPr>
        <p:txBody>
          <a:bodyPr wrap="square"/>
          <a:lstStyle/>
          $titleXml
        </p:txBody>
      </p:sp>
      <p:sp>
        <p:nvSpPr>
          <p:cNvPr id="3" name="Content"/>
          <p:cNvSpPr txBox="1"/>
          <p:nvPr/>
        </p:nvSpPr>
        <p:spPr>
          <a:xfrm>
            <a:off x="685800" y="1524000"/>
            <a:ext cx="10820400" cy="4572000"/>
          </a:xfrm>
          <a:prstGeom prst="rect"><a:avLst/></a:prstGeom>
          <a:noFill/>
          <a:ln><a:noFill/></a:ln>
        </p:spPr>
        <p:txBody>
          <a:bodyPr wrap="square"/>
          <a:lstStyle/>
          $bodyXml
        </p:txBody>
      </p:sp>
    </p:spTree>
  </p:cSld>
  <p:clrMapOvr>
    <a:masterClrMapping/>
  </p:clrMapOvr>
</p:sld>
"@
}

$fullOutputPath = [System.IO.Path]::GetFullPath((Join-Path (Get-Location) $OutputPath))
$stagingRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("tickets-ppt-" + [guid]::NewGuid().ToString("N"))
$zipPath = Join-Path ([System.IO.Path]::GetTempPath()) ("tickets-ppt-" + [guid]::NewGuid().ToString("N") + ".zip")

New-Item -ItemType Directory -Path $stagingRoot -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingRoot "_rels") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingRoot "docProps") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingRoot "ppt\_rels") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingRoot "ppt\slides\_rels") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingRoot "ppt\slideLayouts\_rels") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingRoot "ppt\slideMasters\_rels") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingRoot "ppt\theme") -Force | Out-Null

$created = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$slide1 = New-SlideXml -Title 'Tickets Overview' -Lines @(
    'Positioning: a backend ticketing project for passenger transport scenarios.'
    'Business: supports signup, login, route search, trip search, seat selection, purchase and refund.'
    'Tech stack: Go, Fiber, MySQL, Redis, sqlc and Docker Compose.'
    'Goal: demonstrate concurrency control, consistency guarantees and engineering delivery.'
)

$slide2 = New-SlideXml -Title 'Architecture and Core Design' -Lines @(
    'Data layer: sqlc maps SQL to Go code, while MySQL stores users, routes, trips, seats and tickets.'
    'Consistency: purchase and refund rely on MySQL transactions and conditional updates.'
    'Caching: Cache Aside is used for cities, terminals and trip queries to reduce read pressure.'
    'Flow control: login uses fixed-window rate limiting and purchase uses token-bucket rate limiting.'
    'High concurrency: Redis seat hold plus async queue smoothing reduce seat conflicts and DB spikes.'
)

$slide3 = New-SlideXml -Title 'Testing and Project Highlights' -Lines @(
    'Coverage: includes oversell tests, concurrent purchase tests and auth middleware tests.'
    'Oversell control: verifies that only the allowed number of buyers can succeed under contention.'
    'Security: validates legal token, missing token, wrong type, malformed token and expired token paths.'
    'Engineering: supports Docker Compose startup, DB migration, demo data seeding and static web pages.'
    'Use case: suitable for interviews, coursework and backend practice on caching and concurrency.'
)

$contentTypes = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>
  <Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
  <Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
  <Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>
  <Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>
  <Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>
  <Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>
  <Override PartName="/ppt/slides/slide2.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>
  <Override PartName="/ppt/slides/slide3.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>
</Types>
"@

$rootRels = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
  <Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>
</Relationships>
"@

$appXml = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">
  <Application>Microsoft Office PowerPoint</Application>
  <PresentationFormat>On-screen Show (16:9)</PresentationFormat>
  <Slides>3</Slides>
  <Notes>0</Notes>
  <HiddenSlides>0</HiddenSlides>
  <MMClips>0</MMClips>
  <ScaleCrop>false</ScaleCrop>
  <HeadingPairs>
    <vt:vector size="2" baseType="variant">
      <vt:variant>
        <vt:lpstr>Slides</vt:lpstr>
      </vt:variant>
      <vt:variant>
        <vt:i4>3</vt:i4>
      </vt:variant>
    </vt:vector>
  </HeadingPairs>
  <TitlesOfParts>
    <vt:vector size="3" baseType="lpstr">
      <vt:lpstr>Tickets 项目概览</vt:lpstr>
      <vt:lpstr>系统设计与核心实现</vt:lpstr>
      <vt:lpstr>测试验证与项目亮点</vt:lpstr>
    </vt:vector>
  </TitlesOfParts>
  <Company>OpenAI Codex</Company>
  <LinksUpToDate>false</LinksUpToDate>
  <SharedDoc>false</SharedDoc>
  <HyperlinksChanged>false</HyperlinksChanged>
  <AppVersion>16.0000</AppVersion>
</Properties>
"@

$coreXml = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:dcmitype="http://purl.org/dc/dcmitype/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <dc:title>Tickets 项目介绍</dc:title>
  <dc:subject>Project Introduction</dc:subject>
  <dc:creator>OpenAI Codex</dc:creator>
  <cp:keywords>Go,Fiber,MySQL,Redis,Ticketing</cp:keywords>
  <dc:description>Three-slide project introduction for Tickets.</dc:description>
  <cp:lastModifiedBy>OpenAI Codex</cp:lastModifiedBy>
  <dcterms:created xsi:type="dcterms:W3CDTF">$created</dcterms:created>
  <dcterms:modified xsi:type="dcterms:W3CDTF">$created</dcterms:modified>
</cp:coreProperties>
"@

$presentationXml = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" saveSubsetFonts="1" autoCompressPictures="0">
  <p:sldMasterIdLst>
    <p:sldMasterId id="2147483648" r:id="rId1"/>
  </p:sldMasterIdLst>
  <p:sldIdLst>
    <p:sldId id="256" r:id="rId2"/>
    <p:sldId id="257" r:id="rId3"/>
    <p:sldId id="258" r:id="rId4"/>
  </p:sldIdLst>
  <p:sldSz cx="12192000" cy="6858000"/>
  <p:notesSz cx="6858000" cy="9144000"/>
</p:presentation>
"@

$presentationRels = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/>
  <Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide2.xml"/>
  <Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide3.xml"/>
</Relationships>
"@

$slideMasterXml = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld name="Office Theme">
    <p:bg>
      <p:bgPr>
        <a:solidFill><a:schemeClr val="bg1"/></a:solidFill>
        <a:effectLst/>
      </p:bgPr>
    </p:bg>
    <p:spTree>
      <p:nvGrpSpPr>
        <p:cNvPr id="1" name=""/>
        <p:cNvGrpSpPr/>
        <p:nvPr/>
      </p:nvGrpSpPr>
      <p:grpSpPr>
        <a:xfrm>
          <a:off x="0" y="0"/>
          <a:ext cx="0" cy="0"/>
          <a:chOff x="0" y="0"/>
          <a:chExt cx="0" cy="0"/>
        </a:xfrm>
      </p:grpSpPr>
    </p:spTree>
  </p:cSld>
  <p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>
  <p:sldLayoutIdLst>
    <p:sldLayoutId id="2147483649" r:id="rId1"/>
  </p:sldLayoutIdLst>
  <p:txStyles>
    <p:titleStyle>
      <a:lvl1pPr algn="l"/>
    </p:titleStyle>
    <p:bodyStyle>
      <a:lvl1pPr marL="0" indent="0"/>
    </p:bodyStyle>
    <p:otherStyle>
      <a:defPPr/>
    </p:otherStyle>
  </p:txStyles>
</p:sldMaster>
"@

$slideMasterRels = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/>
</Relationships>
"@

$slideLayoutXml = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldLayout xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" type="titleAndContent" preserve="1">
  <p:cSld name="Title and Content">
    <p:spTree>
      <p:nvGrpSpPr>
        <p:cNvPr id="1" name=""/>
        <p:cNvGrpSpPr/>
        <p:nvPr/>
      </p:nvGrpSpPr>
      <p:grpSpPr>
        <a:xfrm>
          <a:off x="0" y="0"/>
          <a:ext cx="0" cy="0"/>
          <a:chOff x="0" y="0"/>
          <a:chExt cx="0" cy="0"/>
        </a:xfrm>
      </p:grpSpPr>
    </p:spTree>
  </p:cSld>
  <p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr>
</p:sldLayout>
"@

$slideLayoutRels = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/>
</Relationships>
"@

$themeXml = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="Office Theme">
  <a:themeElements>
    <a:clrScheme name="Office">
      <a:dk1><a:srgbClr val="1F2937"/></a:dk1>
      <a:lt1><a:srgbClr val="FFFFFF"/></a:lt1>
      <a:dk2><a:srgbClr val="0C7C73"/></a:dk2>
      <a:lt2><a:srgbClr val="F6EFE3"/></a:lt2>
      <a:accent1><a:srgbClr val="0C7C73"/></a:accent1>
      <a:accent2><a:srgbClr val="D96F32"/></a:accent2>
      <a:accent3><a:srgbClr val="2D7A47"/></a:accent3>
      <a:accent4><a:srgbClr val="617180"/></a:accent4>
      <a:accent5><a:srgbClr val="B7463E"/></a:accent5>
      <a:accent6><a:srgbClr val="9A4D1F"/></a:accent6>
      <a:hlink><a:srgbClr val="0563C1"/></a:hlink>
      <a:folHlink><a:srgbClr val="954F72"/></a:folHlink>
    </a:clrScheme>
    <a:fontScheme name="Office">
      <a:majorFont>
        <a:latin typeface="Microsoft YaHei"/>
        <a:ea typeface="Microsoft YaHei"/>
        <a:cs typeface="Arial"/>
      </a:majorFont>
      <a:minorFont>
        <a:latin typeface="Microsoft YaHei"/>
        <a:ea typeface="Microsoft YaHei"/>
        <a:cs typeface="Arial"/>
      </a:minorFont>
    </a:fontScheme>
    <a:fmtScheme name="Office">
      <a:fillStyleLst>
        <a:solidFill><a:schemeClr val="lt1"/></a:solidFill>
        <a:solidFill><a:schemeClr val="accent1"/></a:solidFill>
        <a:solidFill><a:schemeClr val="accent2"/></a:solidFill>
      </a:fillStyleLst>
      <a:lnStyleLst>
        <a:ln w="9525"><a:solidFill><a:schemeClr val="accent1"/></a:solidFill></a:ln>
        <a:ln w="25400"><a:solidFill><a:schemeClr val="accent2"/></a:solidFill></a:ln>
        <a:ln w="38100"><a:solidFill><a:schemeClr val="accent3"/></a:solidFill></a:ln>
      </a:lnStyleLst>
      <a:effectStyleLst>
        <a:effectStyle><a:effectLst/></a:effectStyle>
        <a:effectStyle><a:effectLst/></a:effectStyle>
        <a:effectStyle><a:effectLst/></a:effectStyle>
      </a:effectStyleLst>
      <a:bgFillStyleLst>
        <a:solidFill><a:schemeClr val="lt1"/></a:solidFill>
        <a:solidFill><a:schemeClr val="lt2"/></a:solidFill>
        <a:solidFill><a:schemeClr val="bg1"/></a:solidFill>
      </a:bgFillStyleLst>
    </a:fmtScheme>
  </a:themeElements>
  <a:objectDefaults/>
  <a:extraClrSchemeLst/>
</a:theme>
"@

$slideRelXml = @"
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
</Relationships>
"@

Write-Utf8File -Path (Join-Path $stagingRoot "[Content_Types].xml") -Content $contentTypes
Write-Utf8File -Path (Join-Path $stagingRoot "_rels\.rels") -Content $rootRels
Write-Utf8File -Path (Join-Path $stagingRoot "docProps\app.xml") -Content $appXml
Write-Utf8File -Path (Join-Path $stagingRoot "docProps\core.xml") -Content $coreXml
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\presentation.xml") -Content $presentationXml
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\_rels\presentation.xml.rels") -Content $presentationRels
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slideMasters\slideMaster1.xml") -Content $slideMasterXml
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slideMasters\_rels\slideMaster1.xml.rels") -Content $slideMasterRels
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slideLayouts\slideLayout1.xml") -Content $slideLayoutXml
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slideLayouts\_rels\slideLayout1.xml.rels") -Content $slideLayoutRels
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\theme\theme1.xml") -Content $themeXml
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slides\slide1.xml") -Content $slide1
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slides\slide2.xml") -Content $slide2
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slides\slide3.xml") -Content $slide3
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slides\_rels\slide1.xml.rels") -Content $slideRelXml
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slides\_rels\slide2.xml.rels") -Content $slideRelXml
Write-Utf8File -Path (Join-Path $stagingRoot "ppt\slides\_rels\slide3.xml.rels") -Content $slideRelXml

if (Test-Path $zipPath) {
    Remove-Item -LiteralPath $zipPath -Force
}

if (Test-Path $fullOutputPath) {
    Remove-Item -LiteralPath $fullOutputPath -Force
}

Compress-Archive -Path (Join-Path $stagingRoot "*") -DestinationPath $zipPath -Force
if (-not (Test-Path $zipPath)) {
    throw "Zip package was not created."
}

[System.IO.File]::Copy($zipPath, $fullOutputPath, $true)

try {
    Remove-Item -LiteralPath $zipPath -Force
} catch {
    Write-Warning "Temporary zip could not be removed: $zipPath"
}
Remove-Item -LiteralPath $stagingRoot -Recurse -Force

Write-Host "Generated PPTX: $fullOutputPath"
