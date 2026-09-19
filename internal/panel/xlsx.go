// xlsx.go 极简 xlsx 写出器（单工作表 + 内联字符串），零第三方依赖。
//
// 为什么手写而不是引库：面板只需要「把一张表导出成 Excel 能打开的 xlsx」，
// 引入 excelize 会带进一大串传递依赖与 CVE 面；xlsx 本质是 zip + 固定几段 XML，
// 这里只写必需部件（Content_Types / rels / workbook / sheet1），Excel、
// WPS、LibreOffice 均可打开。约定：
//   - 所有单元格都是文本（inlineStr）：券码形如 12 位数字，若当数字写会丢前导 0，
//     日期同理（有效期含 "~"），统一按文本最稳；
//   - 文本做 XML 转义并剔除 XML 1.0 非法控制字符（上游文案里偶发）。
package panel

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// writeXLSX 把 header + rows 写成单工作表 xlsx。widths 可选（列宽，字符数口径）。
func writeXLSX(w io.Writer, sheetName string, header []string, rows [][]string, widths []float64) error {
	zw := zip.NewWriter(w)
	if err := writeZipEntry(zw, "[Content_Types].xml", contentTypesXML); err != nil {
		return err
	}
	if err := writeZipEntry(zw, "_rels/.rels", rootRelsXML); err != nil {
		return err
	}
	if err := writeZipEntry(zw, "xl/workbook.xml", workbookXML(xlsxEscape(sheetName))); err != nil {
		return err
	}
	if err := writeZipEntry(zw, "xl/_rels/workbook.xml.rels", workbookRelsXML); err != nil {
		return err
	}
	sheet, err := sheetXML(header, rows, widths)
	if err != nil {
		return err
	}
	if err := writeZipEntry(zw, "xl/worksheets/sheet1.xml", sheet); err != nil {
		return err
	}
	return zw.Close()
}

func writeZipEntry(zw *zip.Writer, name, body string) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, body)
	return err
}

// sheetXML 生成工作表：第一行表头，随后数据行；列宽可选。
func sheetXML(header []string, rows [][]string, widths []float64) (string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	if len(widths) > 0 {
		b.WriteString("<cols>")
		for i, w := range widths {
			if w <= 0 {
				continue
			}
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%s" customWidth="1"/>`, i+1, i+1, formatFloat(w))
		}
		b.WriteString("</cols>")
	}
	b.WriteString("<sheetData>")
	if len(header) > 0 {
		if err := writeRow(&b, 1, header); err != nil {
			return "", err
		}
	}
	for i, row := range rows {
		if err := writeRow(&b, i+2, row); err != nil {
			return "", err
		}
	}
	b.WriteString("</sheetData></worksheet>")
	return b.String(), nil
}

func writeRow(b *strings.Builder, rowNum int, cells []string) error {
	fmt.Fprintf(b, `<row r="%d">`, rowNum)
	for i, cell := range cells {
		ref := colName(i) + fmt.Sprint(rowNum)
		// xml:space="preserve" 保留首尾空格；内容按文本写入。
		fmt.Fprintf(b, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, xlsxEscape(cell))
	}
	b.WriteString("</row>")
	return nil
}

// colName 0 → A、25 → Z、26 → AA（工作表列名）。
func colName(i int) string {
	name := ""
	for i >= 0 {
		name = string(rune('A'+i%26)) + name
		i = i/26 - 1
	}
	return name
}

func formatFloat(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// xlsxEscape 转义 XML 文本并剔除 XML 1.0 不允许的控制字符（保留 \t \n \r）。
func xlsxEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(r)
		case r < 0x20:
			// 丢弃控制字符（Excel 会因此报文件损坏）
		default:
			b.WriteRune(r)
		}
	}
	var out strings.Builder
	if err := xml.EscapeText(&out, []byte(b.String())); err != nil {
		return b.String()
	}
	return out.String()
}

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
	`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
	`<Default Extension="xml" ContentType="application/xml"/>` +
	`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
	`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
	`</Types>`

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`</Relationships>`

const workbookRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
	`</Relationships>`

func workbookXML(sheetName string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<sheets><sheet name="` + sheetName + `" sheetId="1" r:id="rId1"/></sheets></workbook>`
}
