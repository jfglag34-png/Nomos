/* Исходник фикстуры plugins/community/pefile/fixtures/sample.dll.
 *
 * Фикстура — настоящий PE-файл: обёртка проверяется на реальном доноре, а не
 * на подделке. Исходник лежит рядом с бинарником намеренно, чтобы файл можно
 * было пересобрать и сверить, а не принимать на веру:
 *
 *   x86_64-w64-mingw32-gcc -shared -O1 -Wl,--no-insert-timestamp \
 *       -o fixtures/sample.dll fixtures/sample.c
 *
 * --no-insert-timestamp нужен для повторяемости: без него линкер пишет в
 * COFF-заголовок время сборки, и два прогона подряд дают разные байты.
 *
 * Импорты выбраны преднамеренно: printf тянет за собой набор api-ms-win-crt-*
 * (пять DLL, несколько символов в каждой), memset/strcpy/strlen — memcpy,
 * GetTickCount — KERNEL32.dll. То есть в фикстуре есть и DLL с одним
 * импортом, и DLL с десятком: оба края видны в тестах.
 */
#include <string.h>
#include <stdio.h>
#include <windows.h>

__declspec(dllexport) int wedra_fixture_answer(void)
{
    char buf[32];

    memset(buf, 0, sizeof(buf));
    strcpy(buf, "fixture");
    printf("%s %lu\n", buf, (unsigned long)GetTickCount());
    return (int)strlen(buf);
}
